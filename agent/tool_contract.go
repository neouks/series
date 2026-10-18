package agent

import (
	"encoding/json"
	"fmt"
	"reflect"

	actool "github.com/Autumn-27/norma/tool"
)

// ResolveBuiltinTool keeps executable structure code-owned. Persisted metadata
// is not rewritten here; valid scalar defaults and non-protocol prose remain editable.
func ResolveBuiltinTool(t actool.CoreTool, description string, saved map[string]any) actool.CoreTool {
	raw, _ := json.Marshal(t.InputSchema())
	var schema map[string]any
	_ = json.Unmarshal(raw, &schema)
	// Asset selector and pagination instructions are executable contract metadata.
	// Persisted descriptions/defaults may advertise obsolete limit-only calls.
	contractOwned := builtinContractOwned(t.Name())
	if t.Name() != "list_assets" && t.Name() != "check_target_access" {
		mergeToolMetadataMode(schema, saved, !contractOwned)
	}
	// These descriptions contain versioned pagination/write protocols. Old saved
	// text must not replace the current contract (e.g. "all findings").
	if contractOwned {
		description = t.Description()
		if !reflect.DeepEqual(scalarDefaults(schema), scalarDefaults(t.InputSchema())) {
			description += "\n已配置自定义缺省值，实际缺省值以当前参数 Schema 的 default 为准。"
		}
	}
	return DecorateTool(t, description, schema)
}

func builtinContractOwned(name string) bool {
	switch name {
	case "context_search", "context_get", "asset_search", "asset_get", "graph_overview", "traffic_get", "traffic_search", "update_finding_report", "get_finding_traffic", "bind_finding_traffic", "list_finding_deletion_feedback", "check_target_access", "add_intent", "dispatch_intents", "get_finding_retest_context", "list_task_assets", "register_user_target", "insert_assets", "create_custom_tool", "update_custom_tool", "list_facts", "list_findings", "list_task_findings", "list_assets", "node_detail", "get_task_node_detail", "get_worker_output", "get_worker_trace", "list_worker_traces", "search_all_worker_traces", "get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces", "get_task_graph", "add_task_hint", "expand_index", "expand_digest", "record_fact", "report_finding":
		return true
	}
	return false
}

// Protocol descriptions are code-owned at every depth. Retain valid configurable
// defaults, but never restore stale field instructions from a previously seeded schema.
func mergeToolMetadataMode(code, saved map[string]any, descriptions bool) {
	mergeToolMetadataAt(code, saved, descriptions, 0)
}

func mergeToolMetadataAt(code, saved map[string]any, descriptions bool, depth int) {
	if code == nil || saved == nil {
		return
	}
	if desc, ok := saved["description"].(string); ok && descriptions {
		code["description"] = desc
	}
	// injectDefaults only applies top-level scalar fields. Advertising saved
	// nested/array defaults would promise behavior the execution layer lacks.
	typ, _ := code["type"].(string)
	scalar := typ == "string" || typ == "integer" || typ == "number" || typ == "boolean"
	if value, ok := saved["default"]; ok && depth == 1 && scalar {
		raw, err := json.Marshal(value)
		if err == nil && validateBusinessInput(code, raw) == nil {
			code["default"] = value
		}
	}
	props, _ := code["properties"].(map[string]any)
	oldProps, _ := saved["properties"].(map[string]any)
	for key, value := range props {
		child, _ := value.(map[string]any)
		old, _ := oldProps[key].(map[string]any)
		mergeToolMetadataAt(child, old, descriptions, depth+1)
	}
	items, _ := code["items"].(map[string]any)
	oldItems, _ := saved["items"].(map[string]any)
	mergeToolMetadataAt(items, oldItems, descriptions, depth+1)
}

func requireRoleTools(role string, tools []actool.CoreTool) error {
	required := map[string][]string{
		"worker":  {"insert_assets", "record_fact", "report_finding"},
		"planner": {"add_intent", "prove_goal"},
	}[role]
	has := make(map[string]bool, len(tools))
	for _, tool := range tools {
		has[tool.Name()] = true
	}
	for _, name := range required {
		if !has[name] {
			return fmt.Errorf("%s 必需工具 %s 已禁用或未绑定，请修正工具配置", role, name)
		}
	}
	return nil
}
