package agent

import (
	"encoding/json"
	actool "github.com/Autumn-27/norma/tool"
	"sort"
	"testing"
)

// Measures the domain surface, not SDK/MCP schemas or exact model tokens.
func TestToolSurfaceMetrics(t *testing.T) {
	sets := builtinToolsByAgent()
	roles := make([]string, 0, len(sets))
	for role := range sets {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		size := 0
		for _, tool := range sets[role] {
			b, err := json.Marshal(map[string]any{"name": tool.Name(), "description": tool.Description(), "parameters": tool.InputSchema()})
			if err != nil {
				t.Fatal(err)
			}
			size += len(b)
		}
		t.Logf("role=%s tools=%d definition_bytes=%d", role, len(sets[role]), size)
	}
}

// Fixed code-default role surface including SDK tools. Host config/MCPs are
// intentionally absent and measured separately because their bindings vary.
func TestRoleToolSurfaceMetrics(t *testing.T) {
	sets := builtinToolsByAgent()
	for _, role := range []string{"goals", "planner", "mainagent", "worker", "auto", "pentest"} {
		tools := sets[role]
		switch role {
		case "planner":
			tools = append(tools, actool.NewBash())
		case "mainagent", "worker":
			tools = append(tools, workerLocalTools()...)
		case "auto", "pentest":
			tools = append(tools, actool.DefaultTools()...)
		}
		size := 0
		for _, tool := range tools {
			b, e := json.Marshal(map[string]any{"name": tool.Name(), "description": tool.Description(), "parameters": tool.InputSchema()})
			if e != nil {
				t.Fatal(e)
			}
			size += len(b)
		}
		t.Logf("role=%s tools_with_sdk=%d schema_bytes=%d", role, len(tools), size)
	}
}
