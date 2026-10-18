package agent

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

func cloneToolMessages(req llm.CompletionRequest) llm.CompletionRequest {
	req.Messages = append([]llm.Message(nil), req.Messages...)
	for i := range req.Messages {
		req.Messages[i].Content = append([]llm.ContentBlock(nil), req.Messages[i].Content...)
	}
	return req
}

// Only side-effect-free reads are eligible; writes and failed calls are excluded.
func normalizedReadQuery(name string, raw json.RawMessage) string {
	defaults := map[string]any{}
	switch name {
	case "context_search", "context_get", "asset_search", "asset_get", "traffic_search", "traffic_get", "get_worker_trace", "search_all_worker_traces", "get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces":
		// New contracts have explicit continuation positions. Never deduplicate writes.
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return ""
		}
		b, _ := json.Marshal(v)
		return name + ":" + string(b)
	case "list_finding_deletion_feedback":
		defaults = map[string]any{"before": json.Number("0"), "limit": json.Number("20")}
	case "check_target_access":
		q, err := decodeTargetAccess(raw)
		if err != nil {
			return ""
		}
		data, _ := json.Marshal(q)
		return name + ":" + string(data)
	case "list_facts", "list_findings", "list_worker_traces":
		defaults = map[string]any{"limit": json.Number("20"), "before": json.Number("0"), "q": "", "severity": "", "asset_id": json.Number("0")}
	case "list_task_assets":
		defaults = map[string]any{"status": "approved", "q": "", "limit": json.Number("20"), "cursor": "", "summary_only": false}
	case "list_assets":
		defaults = map[string]any{"limit": json.Number("10"), "offset": json.Number("0"), "detail": false, "type": "", "dsl": "", "field": "", "index": json.Number("0"), "text_offset": json.Number("0")}
	case "node_detail", "get_worker_output":
		defaults = map[string]any{"offset": json.Number("0"), "max_chars": json.Number("8000")}
		if name == "node_detail" {
			defaults["field"] = ""
			defaults["index"] = json.Number("0")
		}
	default:
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var input map[string]any
	if dec.Decode(&input) != nil || input == nil {
		return ""
	}
	for k, v := range input {
		defaults[k] = v
	}
	if q, ok := defaults["q"].(string); ok {
		defaults["q"] = strings.TrimSpace(q)
	}
	// Zero limit is the documented default in the handlers.
	if n, ok := defaults["limit"].(json.Number); ok && n == "0" {
		if name == "list_assets" {
			defaults["limit"] = json.Number("10")
		} else {
			defaults["limit"] = json.Number("20")
		}
	}
	data, err := json.Marshal(defaults)
	if err != nil {
		return ""
	}
	return name + ":" + string(data)
}

func deduplicateToolHistory(req llm.CompletionRequest) llm.CompletionRequest {
	type position struct{ m, b int }
	calls := map[string]string{}
	latest := map[string]position{}
	var replaced []position
	for mi, m := range req.Messages {
		for bi, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				call := assetContextCall(b)
				calls[b.ID] = normalizedReadQuery(call.Name, call.Input)
			}
			if b.Type != llm.BlockToolResult || b.IsError || partialReadFailure(calls[b.ToolUseID], b) {
				continue
			}
			key := calls[b.ToolUseID]
			if key == "" {
				continue
			}
			if old, ok := latest[key]; ok {
				replaced = append(replaced, old)
			}
			latest[key] = position{mi, bi}
		}
	}
	if len(replaced) == 0 {
		return req
	}
	req = cloneToolMessages(req)
	for _, pos := range replaced {
		req.Messages[pos.m].Content[pos.b].Content = []llm.ContentBlock{{Type: llm.BlockText, Text: `{"message":"同一查询页已有后续结果，此历史正文已在模型视图中省略"}`}}
	}
	return req
}

// Batched reads intentionally report per-item errors without failing valid
// siblings. Such a response cannot replace an older successful batch in the
// model view. Live authorization still revalidates the retained history.
func partialReadFailure(key string, block llm.ContentBlock) bool {
	if !strings.HasPrefix(key, "context_get:") && !strings.HasPrefix(key, "asset_get:") {
		return false
	}
	for _, c := range block.Content {
		if c.Type != llm.BlockText || !strings.Contains(c.Text, `"error"`) {
			continue
		}
		var result struct {
			Items  []map[string]json.RawMessage `json:"items"`
			Assets []map[string]json.RawMessage `json:"assets"`
		}
		if json.Unmarshal([]byte(c.Text), &result) != nil {
			continue
		}
		for _, row := range append(result.Items, result.Assets...) {
			if raw := row["error"]; len(raw) > 0 && string(raw) != "null" && string(raw) != `""` {
				return true
			}
			for _, key := range []string{"payload", "details"} {
				var value struct {
					Error string `json:"error"`
				}
				if json.Unmarshal(row[key], &value) == nil && value.Error != "" {
					return true
				}
			}
		}
	}
	return false
}
