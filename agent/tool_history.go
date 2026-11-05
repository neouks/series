package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
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
	if len(raw) > historicalReadInputBytes {
		return ""
	}
	defaults := map[string]any{}
	switch name {
	case "RecallMemory", "context_search", "context_get", "asset_search", "asset_get", "traffic_search", "traffic_get", "graph_overview", "get_worker_trace", "search_all_worker_traces", "get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces":
		// New contracts have explicit continuation positions. Never deduplicate writes.
		var v map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&v) != nil || v == nil || dec.Decode(new(any)) != io.EOF {
			return ""
		}
		// These tools accept the same task ID as a JSON integer or decimal
		// string. Normalize identity without merging different task scopes.
		if name == "context_search" || name == "context_get" || name == "graph_overview" {
			if id, ok := v["task_id"].(json.Number); ok && !strings.ContainsAny(id.String(), ".eE+-") {
				v["task_id"] = id.String()
			}
		}
		// Normalize only set-valued fields and aliases the handlers actually
		// accept. Item order, cursors, limits and text offsets remain significant.
		if name == "context_search" {
			if values, ok := v["types"].([]any); ok {
				types := []string{}
				valid := true
				for _, x := range values {
					s, ok := x.(string)
					if !ok {
						valid = false
						break
					}
					types = append(types, s)
				}
				if valid {
					slices.Sort(types)
					types = slices.Compact(types)
					v["types"] = types
				}
			}
		}
		if name == "context_get" {
			if items, ok := v["items"].([]any); ok {
				for _, it := range items {
					item, ok := it.(map[string]any)
					if !ok {
						continue
					}
					if item["view"] == "detail" || item["view"] == "" {
						delete(item, "view")
					}
					for _, key := range []string{"offset", "index", "before"} {
						if n, ok := item[key].(json.Number); ok && n == "0" {
							delete(item, key)
						}
					}
					if item["field"] == "" {
						delete(item, "field")
					}
				}
			}
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
		if retiredRetrievalKind(name) == "" {
			return ""
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var input map[string]any
	if dec.Decode(&input) != nil || input == nil || dec.Decode(new(any)) != io.EOF {
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
	return deduplicateReadHistory(req, 1024, 1024)
}

// Batched reads intentionally report per-item errors without failing valid
// siblings. Such a response cannot replace an older successful batch in the
// model view. Live authorization still revalidates the retained history.
func partialReadFailure(key string, block llm.ContentBlock) bool {
	name, _, _ := strings.Cut(key, ":")
	for _, c := range block.Content {
		if c.Type == llm.BlockText && (name == "graph_overview" || name == "get_task_graph") {
			var status map[string]any
			if json.Unmarshal([]byte(c.Text), &status) == nil && overviewHasFailure(status) {
				return true
			}
		}
		if c.Type == llm.BlockText && retiredRetrievalKind(name) != "" {
			var status struct {
				Error string `json:"error"`
			}
			if json.Unmarshal([]byte(c.Text), &status) == nil && status.Error != "" {
				return true
			}
		}
		if c.Type == llm.BlockText && strings.Contains(c.Text, `"context_unavailable"`) {
			var status struct {
				Unavailable bool `json:"context_unavailable"`
			}
			if json.Unmarshal([]byte(c.Text), &status) == nil && status.Unavailable {
				return true
			}
		}
	}
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

func overviewHasFailure(value any) bool {
	switch x := value.(type) {
	case map[string]any:
		for key, v := range x {
			if (key == "partial" || key == "unavailable") && v == true {
				return true
			}
			if key == "unavailable" {
				if m, ok := v.(map[string]any); ok && len(m) > 0 {
					return true
				}
			}
			if key == "error" || strings.HasSuffix(key, "_error") {
				if s, ok := v.(string); ok && s != "" {
					return true
				}
			}
			if overviewHasFailure(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if overviewHasFailure(v) {
				return true
			}
		}
	}
	return false
}
