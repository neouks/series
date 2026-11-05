package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
)

const retrievalBodyRunes = 24000
const retrievalDirectoryRunes = 4000
const retrievalDirectoryPrefix = "检索历史目录（仅索引，非完整证据）："
const legacyRetrievalContextRule = "检索结果和态势是有界窗口，未展示不代表不存在。优先按资产、意图和字段缩小范围；搜索结果的 read 可直接传给 context_get，从命中附近读取。history_omitted 仅表示历史正文被精简，可按原调用重读；不要把它当作空结果或否定结论。"
const previousRetrievalContextRule = legacyRetrievalContextRule + "较早结果是当时快照；不同结果需结合来源和原始证据核对，不把旧快照当作当前状态。"
const retrievalContextRule = previousRetrievalContextRule + "历史旧工具名仅用于审计；重读使用reread指定的当前工具，旧ID游标不能当作当前搜索cursor。"

func retrievalTool(name string) bool {
	switch name {
	case "context_search", "context_get", "asset_search", "asset_get", "traffic_search", "traffic_get", "graph_overview", "RecallMemory":
		return true
	}
	return retiredRetrievalKind(name) != ""
}

func clearRetrievalDirectory(req llm.CompletionRequest) llm.CompletionRequest {
	return withoutSystemSegments(req, func(s string) bool {
		return strings.HasPrefix(s, retrievalDirectoryPrefix) || s == retrievalContextRule || s == previousRetrievalContextRule || s == legacyRetrievalContextRule
	})
}

// This is a copy-on-write model projection, after live authorization. No ledger,
// transcript, archive or SDK working set is mutated. Repeated calls are stable.
func boundRetrievalHistory(req llm.CompletionRequest) llm.CompletionRequest {
	req = clearRetrievalDirectory(req)
	type result struct {
		m, b, c int
		call    llm.ContentBlock
		text    string
	}
	calls := map[string]llm.ContentBlock{}
	seenCalls := map[string]bool{}
	var results []result
	for mi, m := range req.Messages {
		if m.Role == llm.RoleAssistant || m.Role == "system" {
			clear(calls)
			clear(seenCalls)
		}
		for bi, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				delete(calls, b.ID)
				if b.ID == "" || len(b.ID) > 1024 || seenCalls[b.ID] || len(seenCalls) >= 1024 || len(b.Input) > historicalReadInputBytes {
					continue
				}
				seenCalls[b.ID] = true
				c := assetContextCall(b)
				calls[c.ID] = c
				continue
			}
			if b.Type != llm.BlockToolResult {
				if m.Role == llm.RoleUser {
					clear(calls)
					clear(seenCalls)
				}
				continue
			}
			call := calls[b.ToolUseID]
			delete(calls, b.ToolUseID)
			// Error blocks retain their diagnostics. Partial structured reads may
			// shrink successful data while keeping exact per-item failure metadata.
			if b.Type != llm.BlockToolResult || b.IsError || !retrievalTool(call.Name) {
				continue
			}
			for ci, c := range b.Content {
				if c.Type != llm.BlockText {
					continue
				}
				// Store positions only. Decode one result at a time below rather
				// than retaining all historical evidence trees simultaneously.
				results = append(results, result{mi, bi, ci, call, c.Text})
			}
		}
	}
	if len(results) == 0 {
		req.System = append(req.System, retrievalContextRule)
		return req
	}
	cloned := false
	remaining := retrievalBodyRunes
	entries := []any{}
	byQuery := map[string]map[string]any{}
	directoryTruncated := false
	for i := len(results) - 1; i >= 0; i-- {
		r := results[i]
		value := retrievalValue(r.text, r.call.Name)
		// Refusals, malformed records and dedup placeholders must not become
		// a data-bearing historical directory.
		if value == nil || !retrievalHasData(value) {
			continue
		}
		size := utf8.RuneCountInString(r.text)
		if size <= remaining {
			remaining -= size
			continue
		}
		// Preserve the newest partial preview as whole fields/records. Never let an
		// older small page displace the newer evidence the model is working on.
		entry := retrievalIndex(value, r.call)
		replay := retrievalReplayIndex(r.call, value, entry)
		compact := compactRetrievalResult(value, r.call, replay)
		encoded, _ := json.Marshal(compact)
		if utf8.RuneCount(encoded) <= remaining {
			remaining -= utf8.RuneCount(encoded)
		} else {
			if diagnostics := retrievalDiagnostics(value, r.call.Name); len(diagnostics) > 0 {
				diagnostics["history_omitted"], diagnostics["read_from_original_call"] = true, true
				encoded, _ = json.Marshal(diagnostics)
			} else {
				encoded = []byte(historicalReadOmitted)
			}
		}
		if !cloned {
			req = cloneToolMessages(req)
			cloned = true
		}
		block := &req.Messages[r.m].Content[r.b]
		block.Content = slices.Clone(block.Content)
		block.Content[r.c].Text = string(encoded)
		// Future older pages are indices only once the body window is full.
		remaining = 0
		group := retrievalQueryGroup(r.call)
		if retiredRetrievalKind(r.call.Name) != "" {
			if replay != nil {
				entry["reread"] = replay
			}
			entry["legacy_cursor_not_reusable"] = true
		}
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

func retrievalValue(text, name string) map[string]any {
	var value any
	if decodeStructuredJSON([]byte(text), &value) != nil {
		if name == "RecallMemory" {
			return map[string]any{"preview": text, "legacy_memory": true, "legacy_scan_scope": "unknown", "is_current_evidence": false}
		}
		return nil
	}
	if m, ok := value.(map[string]any); ok {
		return m
	}
	if rows, ok := value.([]any); ok && name == "list_goals" {
		return map[string]any{"goals": rows}
	}
	return nil
}

func retrievalDirectorySize(entries []any, truncated bool) int {
	b, _ := json.Marshal(map[string]any{"entries": entries, "truncated": truncated, "older_calls_remain_in_history": true})
	return utf8.RuneCountInString(retrievalDirectoryPrefix) + utf8.RuneCount(b)
}
func retrievalHasData(m map[string]any) bool {
	for _, key := range []string{"items", "assets", "payload", "details", "parts", "request", "response", "id", "ref", "task", "goals", "recent_facts", "frontier_open", "facts", "findings", "steps", "hits", "works", "members", "final_text", "body", "preview", "content"} {
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
		} else if id, ok := structuredInt64(row["step_id"]); ok && id > 0 && retiredRetrievalKind(call.Name) == "activity" {
			ref["ref"] = fmt.Sprintf("activity:%d", id)
		} else if id, ok := structuredInt64(row["intent_id"]); ok && id > 0 && retiredRetrievalKind(call.Name) == "intent" {
			ref["ref"] = fmt.Sprintf("node:%d", id)
		} else if id, ok := structuredInt64(row["id"]); ok && id > 0 {
			if strings.HasPrefix(call.Name, "asset_") || retiredRetrievalKind(call.Name) == "asset" {
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
		for _, key := range []string{"items", "assets", "parents", "members", "goals", "hints", "running_intents", "open_intents", "recent_done_intents", "recent_facts", "finding_list", "cold_digests", "related_tasks", "facts", "findings", "steps", "hits", "works"} {
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
	for _, key := range []string{"has_more", "next_cursor", "next_offset", "next_before"} {
		if v, ok := value[key]; ok {
			out[key] = v
		}
	}
	return out
}

func compactRetrievalResult(value map[string]any, call llm.ContentBlock, replay map[string]any) map[string]any {
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
				if k == "error" || k == "errors" || k == "unavailable" || strings.HasSuffix(k, "_error") {
					m[k] = v
					continue
				}
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
	if replay != nil {
		out["reread"] = replay
	} else if retiredRetrievalKind(call.Name) != "" {
		out["reread_unavailable"] = "旧调用缺少可可靠转换的身份；请查看原始调用或使用当前检索工具，不要调用已退役工具"
	}
	return out
}
