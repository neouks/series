package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

func legacyReadAnchors(call llm.ContentBlock) []int64 {
	var a struct {
		ID       int64 `json:"id"`
		IntentID int64 `json:"intent_id"`
	}
	if len(call.Input) > historicalReadInputBytes || json.Unmarshal(call.Input, &a) != nil {
		return nil
	}
	switch call.Name {
	case "node_detail", "get_task_node_detail", "expand_digest", "expand_index":
		if a.ID > 0 {
			return []int64{a.ID}
		}
	case "get_worker_output", "get_worker_trace", "get_task_worker_trace":
		if a.IntentID > 0 {
			return []int64{a.IntentID}
		}
	}
	return nil
}

// Historical attribution only. These names are never registered as executable
// aliases, and current-tool rereads still pass current bindings and live access.
func retiredRetrievalKind(name string) string {
	switch name {
	case "list_facts":
		return "fact"
	case "list_findings", "list_task_findings":
		return "finding"
	case "list_goals":
		return "goal"
	case "list_worker_traces", "list_task_worker_traces":
		return "intent"
	case "get_worker_trace", "get_task_worker_trace", "search_all_worker_traces", "search_task_worker_traces":
		return "activity"
	case "node_detail", "get_task_node_detail", "get_worker_output", "expand_digest", "expand_index":
		return "node"
	case "list_assets", "list_untested_assets":
		return "asset"
	case "get_task_graph":
		return "graph"
	case "traffic_blob":
		return "traffic"
	}
	return ""
}

// Never reinterpret legacy ID cursors as today's opaque search cursor. Lists
// reread known returned identities; continuing a search requires a fresh search.
func retrievalReplay(call llm.ContentBlock, value map[string]any) map[string]any {
	return retrievalReplayIndex(call, value, nil)
}

func retrievalReplayIndex(call llm.ContentBlock, value, index map[string]any) map[string]any {
	var args map[string]any
	if len(call.Input) > historicalReadInputBytes || decodeStructuredJSON(call.Input, &args) != nil || args == nil {
		return nil
	}
	if retiredRetrievalKind(call.Name) == "" {
		switch call.Name {
		case "context_get", "asset_get":
			args["max_chars"] = 2000
		case "traffic_get":
			args["max_bytes"] = 2048
		case "context_search", "asset_search", "traffic_search":
			args["limit"] = 1
		}
		return map[string]any{"tool": call.Name, "args": args}
	}
	withTask := func(v map[string]any) map[string]any {
		if task, ok := args["task_id"]; ok {
			v["task_id"] = task
		}
		return v
	}
	readNode := func(ref string, view string) map[string]any {
		item := map[string]any{"ref": ref}
		if view != "" {
			item["view"] = view
		}
		for _, k := range []string{"field", "index", "offset"} {
			if v, ok := args[k]; ok && view != "members" {
				item[k] = v
			}
		}
		if view == "members" {
			if v, ok := args["before"]; ok {
				item["before"] = v
			}
		}
		return map[string]any{"tool": "context_get", "args": withTask(map[string]any{"items": []any{item}, "max_chars": 2000})}
	}
	switch call.Name {
	case "node_detail", "get_task_node_detail", "expand_digest":
		if id, ok := structuredInt64(args["id"]); ok && id > 0 {
			view := ""
			if call.Name == "expand_digest" {
				view = "members"
			}
			return readNode(fmt.Sprintf("node:%d", id), view)
		}
	case "get_worker_output":
		if id, ok := structuredInt64(args["intent_id"]); ok && id > 0 {
			return readNode(fmt.Sprintf("node:%d", id), "output")
		}
	case "get_task_graph":
		return map[string]any{"tool": "graph_overview", "args": withTask(map[string]any{})}
	case "traffic_blob":
		// A filesystem/blob path alone cannot identify a traffic record or part.
		return nil
	}
	if index == nil {
		index = retrievalIndex(value, call)
	}
	items, ids := []any{}, []int64{}
	if retiredRetrievalKind(call.Name) == "asset" {
		if id, ok := structuredInt64(args["id"]); ok && id > 0 {
			ids = append(ids, id)
		}
		if values, ok := args["ids"].([]any); ok {
			for _, v := range values {
				if id, ok := structuredInt64(v); ok && id > 0 {
					ids = append(ids, id)
				}
			}
		}
	}
	for _, v := range index["refs"].([]any) {
		ref := v.(map[string]any)
		if id, ok := structuredInt64(ref["asset_id"]); ok {
			ids = append(ids, id)
			continue
		}
		name, _ := ref["ref"].(string)
		if name == "" {
			if id, ok := structuredInt64(ref["node_id"]); ok {
				name = fmt.Sprintf("node:%d", id)
			}
		}
		if _, _, err := parseContextRef(name); err != nil {
			continue
		}
		item := map[string]any{"ref": name}
		for _, k := range []string{"field", "index", "offset"} {
			if v, ok := ref[k]; ok {
				item[k] = v
			}
		}
		if strings.HasPrefix(name, "activity:") {
			item["field"] = "/detail"
			delete(item, "index")
		}
		items = append(items, item)
		if len(items) == 5 {
			break
		}
	}
	if len(ids) > 0 {
		read := map[string]any{"ids": ids[:min(5, len(ids))], "max_chars": 2000}
		for _, k := range []string{"fields", "field", "index"} {
			if v, ok := args[k]; ok {
				read[k] = v
			}
		}
		if offset, ok := args["text_offset"]; ok {
			read["offset"] = offset
		}
		return map[string]any{"tool": "asset_get", "args": read}
	}
	if len(items) > 0 {
		return map[string]any{"tool": "context_get", "args": withTask(map[string]any{"items": items, "max_chars": 2000})}
	}
	return nil
}
