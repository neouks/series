package server

import (
	"encoding/json"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/traffic"
	"sort"
	"testing"
)

// Fixed fresh-install catalog (no custom/MCP/skills, capture enabled); include
// evidence/reporter bindings, orchestration, platform and SDK tools. Deferred tools contribute only their name to
// the advertised catalogue, not their full JSON schema on each request.
func TestDefaultContextSurfaceMetrics(t *testing.T) {
	s := &Server{}
	type def struct {
		name, desc string
		schema     map[string]any
		roles      []string
	}
	all := map[string]def{}
	for _, seed := range agent.BuiltinToolSeeds() {
		all[seed.Key] = def{seed.Key, seed.Desc, seed.Schema, seed.Agents}
	}
	for _, tool := range append(s.orchestrationTools(), s.platformTools()...) {
		d, ok := all[tool.Name()]
		if !ok {
			d = def{tool.Name(), tool.Description(), tool.InputSchema(), nil}
		}
		if !contains(d.roles, "auto") {
			d.roles = append(d.roles, "auto")
		}
		all[d.name] = d
	}
	for _, tool := range traffic.SeedToolMetas() {
		all[tool.Name()] = def{tool.Name(), tool.Description(), tool.InputSchema(), []string{"worker", "reporter"}}
	}
	for _, key := range []string{"update_finding_report", "context_get", "context_search", "graph_overview"} {
		d := all[key]
		d.roles = append(d.roles, "reporter")
		all[key] = d
	}
	for key, roles := range map[string][]string{
		"get_finding_traffic":  {"auto", "reporter", "worker", "planner", "mainagent", "pentest"},
		"bind_finding_traffic": {"reporter"},
	} {
		d := all[key]
		d.roles = roles
		all[key] = d
	}
	for _, role := range []string{"goals", "planner", "mainagent", "worker", "auto", "pentest", "reporter"} {
		defs := map[string]def{}
		for name, d := range all {
			if contains(d.roles, role) {
				defs[name] = d
			}
		}
		sdk := actool.DefaultTools()
		if role == "goals" {
			sdk = nil
		}
		if role == "planner" {
			sdk = []actool.CoreTool{actool.NewBash()}
		}
		for _, tool := range sdk {
			if (role == "worker" || role == "mainagent") && tool.Name() == "sleep" {
				continue
			}
			defs[tool.Name()] = def{tool.Name(), tool.Description(), tool.InputSchema(), nil}
		}
		keys := []string{}
		for k := range defs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		total, initial, nDeferred := 0, 0, 0
		deferredNames := []string{}
		for _, key := range keys {
			d := defs[key]
			d.schema = applyContextPolicy(actool.Build(actool.Spec{Name: d.name, Schema: d.schema}), defaultContextAccess(role)).InputSchema()
			b, _ := json.Marshal(map[string]any{"name": d.name, "description": d.desc, "parameters": d.schema})
			total += len(b)
			// This is the code-default policy introduced by this change. The baseline
			// copy of this test counts platform schemas directly instead.
			if contains(platformToolKeys, key) {
				deferredNames = append(deferredNames, key)
				nDeferred++
			} else {
				initial += len(b)
			}
		}
		if nDeferred > 0 {
			initial += len(actool.RenderDeferredToolsBlock(deferredNames))
			reg := actool.NewRegistry()
			for _, tool := range []actool.CoreTool{actool.NewSearchExtraTools(reg, deferredNames), actool.NewExecuteExtraTool(reg, nil)} {
				b, _ := json.Marshal(map[string]any{"name": tool.Name(), "description": tool.Description(), "parameters": tool.InputSchema()})
				initial += len(b)
			}
		}

		t.Logf("role=%s catalog=%d all_schema_bytes=%d initial_schema_and_deferred_names_bytes=%d deferred=%d", role, len(keys), total, initial, nDeferred)
	}
}
