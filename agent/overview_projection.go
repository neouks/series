package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"
)

const plannerOverviewRunes = 24000

// Projection never slices serialized records. Each section gets a directory
// entry before any section gets body space; round-robin admission prevents a
// large early section from hiding every later category.
func boundedGraphOverview(data map[string]any, maxRunes, maxBytes int, readTask ...string) map[string]any {
	task := ""
	if len(readTask) > 0 {
		task = readTask[0]
	}
	raw, _ := json.Marshal(data)
	var source map[string]any
	_ = decodeStructuredJSON(raw, &source)
	keys := []string{"task", "asset_approval_counts", "unavailable", "goals", "running_intents", "open_intents", "recent_facts", "finding_list", "cold_digests", "related_tasks", "hints", "recent_done_intents"}
	known := map[string]bool{}
	for _, k := range keys {
		known[k] = true
	}
	var extra []string
	for k := range source {
		if !known[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	keys = append(keys, extra...)
	out := map[string]any{}
	sections := map[string]any{}
	out["sections"] = sections
	type section struct {
		key  string
		rows []any
		list bool
		pos  int
		meta map[string]any
	}
	var all []*section
	for _, k := range keys {
		value, exists := source[k]
		if !exists && !known[k] {
			continue
		}
		rows, list := value.([]any)
		if !list && exists {
			rows = []any{value}
		}
		meta := map[string]any{"shown": 0, "has_more": len(rows) > 0, "scope": "window"}
		if read := overviewRead(k); read != nil {
			meta["read"] = read
		}
		if errs, ok := source["unavailable"].(map[string]any); ok && errs[k] != nil {
			meta["unavailable"] = true
			delete(meta, "shown")
			delete(meta, "has_more")
		} else if !exists && known[k] {
			meta["not_provided"] = true
			delete(meta, "shown")
			delete(meta, "has_more")
		}
		scopeOverviewReads(meta, task)
		sections[k] = meta
		if list {
			out[k] = []any{}
		}
		all = append(all, &section{k, rows, list, 0, meta})
	}
	fits := func() bool {
		b, _ := json.Marshal(out)
		return utf8.RuneCount(b) <= maxRunes && (maxBytes <= 0 || len(b) <= maxBytes)
	}
	// Metadata alone must also fit. A caller with an unusually tiny output cap
	// receives an explicit retry, never a misleading empty overview.
	if !fits() {
		return map[string]any{"context_unavailable": true, "reason": "overview_budget", "message": "态势目录超过输出预算；请提高工具输出上限或按类型使用 context_search"}
	}
	// Track incremental JSON sizes; do not repeatedly serialize the growing
	// overview for every candidate (large graphs otherwise allocate quadratically).
	base, _ := json.Marshal(out)
	usedRunes, usedBytes := utf8.RuneCount(base), len(base)
	reserve := 96 * len(all) // shown/has_more/omitted metadata growth
	for {
		advanced := false
		for _, s := range all {
			if s.pos >= len(s.rows) {
				continue
			}
			row := compactOverviewRecord(s.rows[s.pos], s.key)
			scopeOverviewReads(row, task)
			encoded, _ := json.Marshal(row)
			deltaRunes, deltaBytes := utf8.RuneCount(encoded), len(encoded)
			if s.list {
				if s.pos > 0 {
					deltaRunes++
					deltaBytes++
				}
			} else {
				key, _ := json.Marshal(s.key)
				deltaRunes += utf8.RuneCount(key) + 2
				deltaBytes += len(key) + 2
			}
			if usedRunes+deltaRunes+reserve > maxRunes || (maxBytes > 0 && usedBytes+deltaBytes+reserve > maxBytes) {
				continue
			}
			if s.list {
				out[s.key] = append(out[s.key].([]any), row)
			} else {
				out[s.key] = row
			}
			usedRunes += deltaRunes
			usedBytes += deltaBytes
			s.pos++
			advanced = true
			s.meta["shown"] = s.pos
			s.meta["has_more"] = overviewHasMore(source, s.key, s.pos, len(s.rows))
		}
		if !advanced {
			break
		}
	}
	for _, s := range all {
		if s.pos < len(s.rows) {
			s.meta["has_more"] = true
			s.meta["omitted_in_window"] = len(s.rows) - s.pos
		} else if s.meta["unavailable"] != true && s.meta["not_provided"] != true {
			s.meta["has_more"] = overviewHasMore(source, s.key, s.pos, len(s.rows))
		}
	}
	// Reserve metadata growth without ever dropping a whole section directory.
	for !fits() {
		removed := false
		for i := len(all) - 1; i >= 0; i-- {
			s := all[i]
			if s.pos == 0 {
				continue
			}
			s.pos--
			if s.list {
				out[s.key] = out[s.key].([]any)[:s.pos]
			} else {
				delete(out, s.key)
			}
			s.meta["shown"] = s.pos
			s.meta["has_more"] = true
			s.meta["omitted_in_window"] = len(s.rows) - s.pos
			removed = true
			break
		}
		if !removed {
			return map[string]any{"context_unavailable": true, "reason": "overview_budget"}
		}
	}
	return out
}

func overviewHasMore(source map[string]any, key string, shown, available int) any {
	if shown < available {
		return true
	}
	counts := map[string]string{"open_intents": "frontier_open", "finding_list": "findings_total", "recent_facts": "facts", "recent_done_intents": "done_intents_total"}
	if n, ok := structuredInt64(source[counts[key]]); ok {
		return int64(shown) < n
	}
	if key == "cold_digests" {
		if more, ok := source["cold_digests_more"].([]any); ok && len(more) > 0 {
			return true
		}
	}
	switch key {
	case "goals", "hints", "running_intents", "recent_done_intents", "cold_digests", "related_tasks":
		return nil
	}
	return false
}

func overviewRead(key string) map[string]any {
	kind, state := "", ""
	switch key {
	case "task":
		kind, state = "fact", "origin"
	case "goals":
		kind = "goal"
	case "hints":
		kind = "hint"
	case "running_intents":
		kind, state = "intent", "running"
	case "open_intents":
		kind, state = "intent", "open"
	case "recent_done_intents", "recent_intent_results":
		kind = "intent"
	case "recent_facts":
		kind = "fact"
	case "finding_list", "recent_findings":
		kind = "finding"
	case "cold_digests", "cold_digests_more":
		kind, state = "digest", "active"
	case "related_tasks":
		return map[string]any{"tool": "context_search", "args": map[string]any{"types": []string{"fact", "finding", "digest"}, "limit": 10}}
	default:
		return nil
	}
	args := map[string]any{"types": []string{kind}, "limit": 10}
	if state != "" {
		args["state"] = state
	}
	return map[string]any{"tool": "context_search", "args": args}
}

func compactOverviewRecord(value any, section string) any {
	row, ok := value.(map[string]any)
	if !ok {
		return value
	}
	out := map[string]any{}
	omitted := map[string]any{}
	id, _ := structuredInt64(row["id"])
	for k, v := range row {
		b, _ := json.Marshal(v)
		// Keep identities and small fields whole. Large fields have an executable
		// field read, not an ambiguous prose suffix or a fabricated conclusion.
		if utf8.RuneCount(b) > 800 || (section == "related_tasks" && utf8.RuneCount(b) > 240) {
			if id > 0 && (k == "parents" || k == "yields") {
				omitted[k] = map[string]any{"tool": "context_get", "args": map[string]any{"items": []contextRead{{Ref: fmt.Sprintf("node:%d", id), View: "parents"}}}}
				if k == "yields" {
					omitted[k] = map[string]any{"tool": "context_search", "args": map[string]any{"types": []string{"fact", "finding"}, "intent_id": id, "limit": 10}}
				}
			} else if id > 0 {
				omitted[k] = map[string]any{"tool": "context_get", "args": map[string]any{"items": []contextRead{{Ref: fmt.Sprintf("node:%d", id), Field: "/" + escapePointer(k)}}}}
			} else if read := overviewRead(k); read != nil {
				omitted[k] = read
			} else if section == "task" {
				omitted[k] = overviewRead("task")
			} else {
				omitted[k] = map[string]any{"omitted": true}
			}
			continue
		}
		out[k] = v
	}
	if len(omitted) > 0 {
		out["omitted_fields"] = omitted
	}
	return out
}

// Propagate the caller's authorized scope. source_task_id is provenance only.
func scopeOverviewReads(value any, task string) {
	if task == "" {
		return
	}
	switch row := value.(type) {
	case map[string]any:
		if name, ok := row["tool"].(string); ok && (name == "context_search" || name == "context_get") {
			if args, ok := row["args"].(map[string]any); ok {
				args["task_id"] = task
			}
		}
		for _, v := range row {
			scopeOverviewReads(v, task)
		}
	case []any:
		for _, v := range row {
			scopeOverviewReads(v, task)
		}
	}
}
