package agent

import (
	"crypto/sha256"
	"encoding/json"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
)

// Exact-result factoring for nonstructured execution history. Calls and result
// blocks stay paired, no writes are replayed and the latest diagnostics remain.
// Unknown, multimodal, JSON, unpaired and oversized results remain unchanged.
func deduplicatePlainHistory(req llm.CompletionRequest) llm.CompletionRequest {
	type point struct {
		m, b, callAt int
		call         llm.ContentBlock
		result       llm.ContentBlock
	}
	points := []point{}
	assistant := []int{}
	latestError := -1
	for i := 0; i < len(req.Messages); i++ {
		m := req.Messages[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		calls := map[string]llm.ContentBlock{}
		valid := true
		for _, c := range m.Content {
			if c.Type == llm.BlockToolUse {
				if c.ID == "" {
					valid = false
				}
				if _, ok := calls[c.ID]; ok {
					valid = false
				}
				calls[c.ID] = assetContextCall(c)
			}
		}
		if !valid || len(calls) == 0 || len(calls) > 1024 {
			continue
		}
		batch := []point{}
		seen := map[string]bool{}
		for j := i + 1; j < len(req.Messages); j++ {
			next := req.Messages[j]
			if next.Role != llm.RoleUser && next.Role != "tool" {
				break
			}
			pure := len(next.Content) > 0
			for _, r := range next.Content {
				pure = pure && r.Type == llm.BlockToolResult
			}
			if !pure {
				break
			}
			for k, r := range next.Content {
				call, ok := calls[r.ToolUseID]
				if !ok || seen[r.ToolUseID] {
					valid = false
				}
				seen[r.ToolUseID] = true
				batch = append(batch, point{j, k, i, call, r})
			}
		}
		if !valid || len(seen) != len(calls) {
			continue
		}
		assistant = append(assistant, i)
		points = append(points, batch...)
		if len(points) > 1024 {
			copy(points, points[len(points)-1024:])
			points = points[:1024]
		}
		if len(assistant) > 1024 {
			copy(assistant, assistant[len(assistant)-1024:])
			assistant = assistant[:1024]
		}
	}
	if len(assistant) < 3 {
		return req
	}
	cutoff := assistant[len(assistant)-2]
	for i, p := range points {
		if p.result.IsError {
			latestError = i
		}
	}
	latest := map[[32]byte]string{}
	changed := false
	for i := len(points) - 1; i >= 0; i-- {
		p := points[i]
		if len(latest) >= 1024 {
			break
		}
		switch p.call.Name {
		case "Bash", "Monitor", "Read", "Grep", "Glob", "LS":
		default:
			continue
		}
		if !json.Valid(p.call.Input) || len(p.call.Input) > 64<<10 || len(p.result.Content) != 1 || p.result.Content[0].Type != llm.BlockText {
			continue
		}
		text := p.result.Content[0].Text
		plain := noaadapter.StripRefTag(text)
		if len(plain) < 512 || len(plain) > 256<<10 || json.Valid([]byte(plain)) {
			continue
		}
		raw, _ := json.Marshal([]any{p.call.Name, json.RawMessage(p.call.Input), p.result.IsError, plain})
		key := sha256.Sum256(raw)
		if id, ok := latest[key]; ok && p.callAt < cutoff && i != latestError {
			if !changed {
				req = cloneToolMessages(req)
				changed = true
			}
			receipt, _ := json.Marshal(map[string]any{"history_duplicate": true, "same_as_tool_call_id": id, "original_call_id": p.result.ToolUseID, "is_error": p.result.IsError, "note": "此调用的文本结果与后续指定调用逐字相同；原调用和审计保留，不代表未执行。"})
			req.Messages[p.m].Content[p.b].Content = append([]llm.ContentBlock(nil), req.Messages[p.m].Content[p.b].Content...)
			req.Messages[p.m].Content[p.b].Content[0].Text = string(receipt) + text[len(plain):]
		} else if p.callAt >= cutoff || i == latestError {
			latest[key] = p.result.ToolUseID
		}
	}
	return req
}
