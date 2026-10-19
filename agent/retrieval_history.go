package agent

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
)

const retrievalBodyRunes = 24000
const retrievalDirectoryRunes = 4000
const retrievalDirectoryPrefix = "检索历史目录（仅索引，非完整证据）："
const retrievalContextRule = "检索结果和态势是有界窗口，未展示不代表不存在。优先按资产、意图和字段缩小范围；搜索结果的 read 可直接传给 context_get，从命中附近读取。history_omitted 仅表示历史正文被精简，可按原调用重读；不要把它当作空结果或否定结论。"

func retrievalTool(name string) bool {
	switch name {
	case "context_search", "context_get", "asset_search", "asset_get", "traffic_search", "traffic_get", "graph_overview":
		return true
	}
	return false
}

func clearRetrievalDirectory(req llm.CompletionRequest) llm.CompletionRequest {
	system := make([]string, 0, len(req.System))
	for _, s := range req.System {
		if !strings.HasPrefix(s, retrievalDirectoryPrefix) && s != retrievalContextRule {
			system = append(system, s)
		}
	}
	req.System = system
	return req
}

// This is a copy-on-write model projection, after live authorization. No ledger,
// transcript, archive or SDK working set is mutated. Repeated calls are stable.
func boundRetrievalHistory(req llm.CompletionRequest) llm.CompletionRequest {
	req = clearRetrievalDirectory(req)
	type result struct {
		m, b, c int
		call    llm.ContentBlock
		value   map[string]any
		text    string
	}
	calls := map[string]llm.ContentBlock{}
	var results []result
	for mi, m := range req.Messages {
		for bi, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				c := assetContextCall(b)
				calls[c.ID] = c
			}
			call := calls[b.ToolUseID]
			// Failed/partially failed responses retain their diagnostics and protocol.
			if b.Type != llm.BlockToolResult || b.IsError || !retrievalTool(call.Name) || partialReadFailure(normalizedReadQuery(call.Name, call.Input), b) {
				continue
			}
			for ci, c := range b.Content {
				if c.Type != llm.BlockText {
					continue
				}
				var value map[string]any
				if decodeStructuredJSON([]byte(c.Text), &value) != nil || value == nil {
					continue
				}
				// Authorization refusals, malformed-history notices and dedup placeholders
				// have no data and must not become a data-bearing historical directory.
				if !retrievalHasData(value) {
					continue
				}
				results = append(results, result{mi, bi, ci, call, value, c.Text})
			}
		}
	}
	if len(results) == 0 {
		req.System = append(req.System, retrievalContextRule)
		return req
	}
	req = cloneToolMessages(req)
	remaining := retrievalBodyRunes
	entries := []any{}
	byQuery := map[string]map[string]any{}
	directoryTruncated := false
	for i := len(results) - 1; i >= 0; i-- {
		r := results[i]
		size := utf8.RuneCountInString(r.text)
		if size <= remaining {
			remaining -= size
			continue
		}
		// Preserve the newest partial preview as whole fields/records. Never let an
		// older small page displace the newer evidence the model is working on.
		compact := compactRetrievalResult(r.value, r.call)
		encoded, _ := json.Marshal(compact)
		if utf8.RuneCount(encoded) <= remaining {
			remaining -= utf8.RuneCount(encoded)
		} else {
			encoded = []byte(`{"history_omitted":true,"read_from_original_call":true}`)
		}
		block := &req.Messages[r.m].Content[r.b]
		block.Content = slices.Clone(block.Content)
		block.Content[r.c].Text = string(encoded)
		// Future older pages are indices only once the body window is full.
		remaining = 0
		group := retrievalQueryGroup(r.call)
		entry := retrievalIndex(r.value, r.call)
		entry["query_id"] = group
		if old := byQuery[group]; old != nil {
			// Newest continuation stays authoritative; merge only unique references.
			merged := append([]any(nil), old["refs"].([]any)...)
			seen := map[string]bool{}
			for _, v := range merged {
				b, _ := json.Marshal(v)
				seen[string(b)] = true
			}
			for _, v := range entry["refs"].([]any) {
				b, _ := json.Marshal(v)
				if !seen[string(b)] {
					merged = append(merged, v)
					seen[string(b)] = true
				}
			}
			candidate := map[string]any{}
			for k, v := range old {
				candidate[k] = v
			}
			candidate["refs"] = merged
			oldRefs := old["refs"]
			old["refs"] = candidate["refs"]
			if retrievalDirectorySize(entries, true) > retrievalDirectoryRunes {
				old["refs"] = oldRefs
				directoryTruncated = true
			}
		} else {
			trial := append(append([]any(nil), entries...), entry)
			if retrievalDirectorySize(trial, true) <= retrievalDirectoryRunes {
				entries = trial
				byQuery[group] = entry
			} else {
				directoryTruncated = true
			}
		}
	}
	if len(entries) > 0 || directoryTruncated {
		b, _ := json.Marshal(map[string]any{"entries": entries, "truncated": directoryTruncated, "older_calls_remain_in_history": true})
		req.System = append(req.System, retrievalDirectoryPrefix+string(b))
	}
	req.System = append(req.System, retrievalContextRule)
	return req
}

func retrievalDirectorySize(entries []any, truncated bool) int {
	b, _ := json.Marshal(map[string]any{"entries": entries, "truncated": truncated, "older_calls_remain_in_history": true})
	return utf8.RuneCountInString(retrievalDirectoryPrefix) + utf8.RuneCount(b)
}
func retrievalHasData(m map[string]any) bool {
	for _, key := range []string{"items", "assets", "payload", "details", "parts", "request", "response", "id", "ref", "task", "goals", "recent_facts", "frontier_open"} {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}
func retrievalQueryGroup(call llm.ContentBlock) string {
	var input map[string]any
	_ = decodeStructuredJSON(call.Input, &input)
	delete(input, "cursor")
	delete(input, "offset")
	delete(input, "index")
	delete(input, "limit")
	delete(input, "max_chars")
	delete(input, "max_bytes")
	if items, ok := input["items"].([]any); ok {
		for _, v := range items {
			if m, ok := v.(map[string]any); ok {
				for _, k := range []string{"offset", "index", "before", "max_chars"} {
					delete(m, k)
				}
			}
		}
	}
	return querySignature([]any{call.Name, input})
}

func retrievalIndex(value map[string]any, call llm.ContentBlock) map[string]any {
	out := map[string]any{"tool": call.Name, "call_id": call.ID, "refs": []any{}}
	var input map[string]any
	_ = decodeStructuredJSON(call.Input, &input)
	if task, ok := input["task_id"]; ok {
		out["task_id"] = task
	}
	refs := []any{}
	seen := map[string]bool{}
	var visit func(map[string]any, map[string]any)
	visit = func(row, selectors map[string]any) {
		ref := map[string]any{}
		if id, ok := structuredInt64(row["asset_id"]); ok && id > 0 {
			ref["asset_id"] = id
		} else if text, ok := row["ref"].(string); ok {
			if _, _, err := parseContextRef(text); err == nil {
				ref["ref"] = text
			}
		} else if id, ok := structuredInt64(row["id"]); ok && id > 0 {
			if strings.HasPrefix(call.Name, "asset_") {
				ref["asset_id"] = id
			} else {
				ref["node_id"] = id
			}
		} else if id, ok := row["id"].(string); ok && strings.HasPrefix(call.Name, "traffic_") {
			ref["traffic_id"] = id
		}
		if len(ref) > 0 {
			// Retain exact field/window selectors, not just a node identity. Different
			// pages of the same evidence must remain individually recoverable.
			for _, key := range []string{"field", "fields", "view", "index", "offset", "before", "part"} {
				if v, ok := selectors[key]; ok {
					ref[key] = v
				}
			}
			for _, contentKey := range []string{"payload", "details"} {
				if payload, ok := row[contentKey].(map[string]any); ok {
					for _, key := range []string{"offset", "next_offset", "next_index"} {
						if v, ok := payload[key]; ok {
							ref[key] = v
						}
					}
				}
			}
			if parts, ok := row["parts"].([]any); ok {
				positions := []any{}
				for _, part := range parts {
					if m, ok := part.(map[string]any); ok {
						p := map[string]any{}
						for _, key := range []string{"part", "offset", "next_offset"} {
							if v, ok := m[key]; ok {
								p[key] = v
							}
						}
						positions = append(positions, p)
					}
				}
				ref["parts"] = positions
			}
			for _, key := range []string{"source_task_id", "node_id", "match_field", "match_offset", "offset", "next_offset", "next_index", "next_before"} {
				if v, ok := row[key]; ok {
					ref[key] = v
				}
			}
			b, _ := json.Marshal(ref)
			if !seen[string(b)] {
				refs = append(refs, ref)
				seen[string(b)] = true
			}
		}
		for _, key := range []string{"items", "assets", "parents", "members", "goals", "hints", "running_intents", "open_intents", "recent_done_intents", "recent_facts", "finding_list", "cold_digests", "related_tasks"} {
			if list, ok := row[key].([]any); ok {
				for i, v := range list {
					if m, ok := v.(map[string]any); ok {
						selection := input
						// context_get returns items in input order. The same ref can
						// appear with different fields/offsets in one batch.
						if key == "items" && call.Name == "context_get" {
							if items, ok := input["items"].([]any); ok && i < len(items) {
								if request, ok := items[i].(map[string]any); ok && request["ref"] == m["ref"] {
									selection = request
								}
							}
						}
						visit(m, selection)
					}
				}
			}
		}
	}
	visit(value, input)
	out["refs"] = refs
	for _, key := range []string{"has_more", "next_cursor", "next_offset"} {
		if v, ok := value[key]; ok {
			out[key] = v
		}
	}
	return out
}

func compactRetrievalResult(value map[string]any, call llm.ContentBlock) map[string]any {
	// All original record IDs and cursors survive. Omitted fields are re-read
	// from the same call with a smaller window, so no rows are silently skipped.
	out := map[string]any{"history_omitted": true, "read_from_original_call": true, "next_is_after_original_page": true}
	var trim func(any) any
	trim = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			m := map[string]any{}
			var omitted []string
			for k, v := range x {
				if k == "next" || k == "read" {
					m[k] = v
					continue
				}
				if s, ok := v.(string); ok && utf8.RuneCountInString(s) > 160 {
					omitted = append(omitted, k)
					continue
				}
				// An explicit auth projection is evidence, never directory material.
				if k == "auth" {
					omitted = append(omitted, k)
					continue
				}
				m[k] = trim(v)
			}
			if len(omitted) > 0 {
				slices.Sort(omitted)
				m["history_omitted_fields"] = omitted
			}
			return m
		case []any:
			r := make([]any, len(x))
			for i, v := range x {
				r[i] = trim(v)
			}
			return r
		default:
			return v
		}
	}
	for k, v := range trim(value).(map[string]any) {
		out[k] = v
	}
	var replay map[string]any
	if decodeStructuredJSON(call.Input, &replay) == nil {
		switch call.Name {
		case "context_get", "asset_get":
			replay["max_chars"] = 2000
		case "traffic_get":
			replay["max_bytes"] = 2048
		case "context_search", "asset_search", "traffic_search":
			replay["limit"] = 1
		}
		out["reread"] = map[string]any{"tool": call.Name, "args": replay}
	}
	return out
}
