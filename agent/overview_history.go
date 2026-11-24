package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Autumn-27/norma/llm"
)

var reusableOverviewFields = []string{
	"task", "goals", "hints", "open_intents", "running_intents",
	"recent_done_intents", "recent_facts", "finding_list", "related_tasks",
	"cold_digests", "coverage", "asset_approval_counts",
}

type overviewField struct {
	value  []byte
	callID string
}

// Reuse only complete, successful fields already present in a newer model-view
// result. The audit history and authorization projection have run before this.
func reuseOverviewFields(req llm.CompletionRequest) llm.CompletionRequest {
	type position struct{ message, block, content int }
	type candidate struct {
		position
		callID, scope string
		body          string
	}
	calls := map[string]llm.ContentBlock{}
	seen := map[string]bool{}
	var candidates []candidate
	for mi, message := range req.Messages {
		if message.Role == llm.RoleAssistant || message.Role == "system" {
			clear(calls)
			clear(seen)
		}
		for bi, block := range message.Content {
			if block.Type == llm.BlockToolUse {
				delete(calls, block.ID)
				if block.ID == "" || len(block.ID) > 1024 || seen[block.ID] || len(seen) >= 1024 || len(block.Input) > historicalReadInputBytes {
					continue
				}
				seen[block.ID] = true
				call := assetContextCall(block)
				if call.Name == "graph_overview" {
					calls[block.ID] = call
				}
				continue
			}
			if block.Type != llm.BlockToolResult {
				if message.Role == llm.RoleUser {
					clear(calls)
					clear(seen)
				}
				continue
			}
			call, ok := calls[block.ToolUseID]
			delete(calls, block.ToolUseID)
			if !ok || block.IsError || len(block.Content) != 1 || block.Content[0].Type != llm.BlockText {
				continue
			}
			body := block.Content[0].Text
			if len(body) > 256<<10 {
				continue
			}
			var value map[string]json.RawMessage
			if json.Unmarshal([]byte(body), &value) != nil || value["history_omitted"] != nil {
				continue
			}
			var decoded map[string]any
			if json.Unmarshal([]byte(body), &decoded) != nil || overviewHasFailure(decoded) {
				continue
			}
			scope := overviewScope(call, value)
			if scope != "" {
				candidates = append(candidates, candidate{position{mi, bi, 0}, block.ToolUseID, scope, body})
			}
		}
	}
	latest := map[string]map[string]overviewField{}
	cloned := false
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(c.body), &fields) != nil {
			continue
		}
		if latest[c.scope] == nil {
			latest[c.scope] = map[string]overviewField{}
		}
		changed := false
		for _, name := range reusableOverviewFields {
			raw, exists := fields[name]
			if !exists || len(raw) < 256 {
				continue
			}
			canonical := bytes.TrimSpace(raw)
			if newer, ok := latest[c.scope][name]; ok && bytes.Equal(canonical, newer.value) {
				ref, _ := json.Marshal(map[string]any{"snapshot_reuse": map[string]string{"tool_use_id": newer.callID, "field": name}})
				if len(ref)+64 < len(raw) {
					fields[name] = ref
					changed = true
				}
			} else {
				latest[c.scope][name] = overviewField{slices.Clone(canonical), c.callID}
			}
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(fields)
		if err != nil || len(encoded) >= len(c.body) {
			continue
		}
		if !cloned {
			req = cloneToolMessages(req)
			cloned = true
		}
		result := &req.Messages[c.message].Content[c.block]
		result.Content = slices.Clone(result.Content)
		result.Content[c.content].Text = string(encoded)
	}
	return req
}

func overviewScope(call llm.ContentBlock, value map[string]json.RawMessage) string {
	var source json.Number
	if json.Unmarshal(value["source_task_id"], &source) == nil {
		return "task:" + source.String()
	}
	var input struct {
		TaskID json.Number `json:"task_id"`
	}
	if json.Unmarshal(call.Input, &input) == nil && input.TaskID != "" {
		return "task:" + input.TaskID.String()
	}
	if len(call.Input) == 0 || string(call.Input) == "{}" {
		return "local"
	}
	return fmt.Sprintf("input:%s", call.Input)
}
