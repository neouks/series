package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestContextPolicySchemaAndRuntimeBoundaries(t *testing.T) {
	ts := agent.NewToolSet(nil, "test")
	narrow := applyContextPolicy(ts.ContextSearchTool(), agent.ContextAccess{Local: []string{"finding"}})
	props := narrow.InputSchema()["properties"].(map[string]any)
	if props["task_id"] != nil {
		t.Fatal("cross task selector exposed")
	}
	enum := props["types"].(map[string]any)["items"].(map[string]any)["enum"].([]string)
	if !slices.Equal(enum, []string{"finding"}) {
		t.Fatal(enum)
	}
	for _, raw := range []string{`{"types":["fact"]}`, `{"types":["activity"]}`, `{"task_id":"123","types":["finding"]}`} {
		r, e := narrow.Call(t.Context(), json.RawMessage(raw), nil)
		if e != nil || !r.IsError {
			t.Fatal(raw, r, e)
		}
	}
	output := applyContextPolicy(ts.ContextGetTool(), agent.ContextAccess{Local: []string{"output"}})
	props = output.InputSchema()["properties"].(map[string]any)
	views := props["items"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["view"].(map[string]any)["enum"].([]string)
	if !slices.Equal(views, []string{"output"}) {
		t.Fatal(views)
	}
	cross := applyContextPolicy(ts.ContextSearchTool(), agent.ContextAccess{Cross: []string{"finding"}})
	if cross.InputSchema()["properties"].(map[string]any)["task_id"] == nil {
		t.Fatal("lost cross task grant")
	}
}

func TestWorkerContextSearchSchemaRejectsUnsupportedTypeBeforeExecution(t *testing.T) {
	ts := agent.NewToolSet(nil, "worker")
	tool := applyContextPolicy(agent.ResolveBuiltinTool(ts.ContextSearchTool(), "", nil), defaultContextAccess("worker"))
	if strings.Contains(tool.Description(), "默认查询有权限的 fact/finding/intent") {
		t.Fatal("role description still suggests types unavailable to Worker")
	}
	// Without the evidence workflow decorator this used to pass preflight, then
	// fail in the handler with a different error. Both paths must agree.
	for _, wrapped := range []bool{false, true} {
		candidate := tool
		if wrapped {
			candidate = agent.DecorateTool(tool, tool.Description(), tool.InputSchema())
		}
		input := json.RawMessage(`{"q":"hsaxh","types":["fact","finding"],"limit":50}`)
		_, err := agent.PrepareToolInput(candidate, input)
		if err == nil || !strings.Contains(err.Error(), `allowed values: ["finding","activity"]`) {
			t.Fatalf("wrapped=%v: missing role-specific choices: %v", wrapped, err)
		}
		result, err := candidate.Call(t.Context(), input, nil)
		if err != nil || !result.IsError || !strings.Contains(result.Flatten(), `allowed values: ["finding","activity"]`) {
			t.Fatalf("wrapped=%v: execution boundary disagrees: %v %s", wrapped, err, result.Flatten())
		}
		for _, permitted := range []string{`{"q":"hsaxh","limit":50}`, `{"q":"hsaxh","types":["finding"],"limit":50}`} {
			if _, err := agent.PrepareToolInput(candidate, json.RawMessage(permitted)); err != nil {
				t.Fatalf("valid query refused: %v", err)
			}
		}
	}
}

func TestContextTaskSelectorThroughPolicyAndCatalog(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	task, err := pg.CreateTask("context task selector", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTask(task.ID)
	es := pg.Exploration(task.ExplorationID)
	node, err := es.AddNode(db.KindFinding, map[string]any{"summary": "SELECTOR_FINDING"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := agent.NewToolSet(es, "worker")
	ts.SetTaskID(task.ID)
	policy := agent.ContextAccess{Local: []string{"finding", "detail", "overview"}}
	var overview actool.CoreTool
	for _, tool := range ts.AllDomainTools() {
		if tool.Name() == "graph_overview" {
			overview = tool
		}
	}
	for _, spec := range []struct {
		tool   actool.CoreTool
		params string
	}{
		{ts.ContextSearchTool(), `"types":["finding"],"limit":20`},
		{ts.ContextSearchTool(), `"limit":20`}, // default types must use local grants too
		{ts.ContextGetTool(), fmt.Sprintf(`"items":[{"ref":"node:%d"}]`, node)},
		{overview, ``},
	} {
		for _, exposeCross := range []bool{false, true} {
			p := policy
			if exposeCross {
				// An unrelated cross capability exposes task_id but cannot grant finding reads.
				p.Cross = []string{"activity"}
			}
			// Existing stored string schema must not restore the old type restriction.
			tool := applyContextPolicy(agent.ResolveBuiltinTool(spec.tool, "old", map[string]any{
				"type": "object", "properties": map[string]any{"task_id": map[string]any{"type": "string"}},
			}), p)
			inputs := []struct {
				id      string
				allowed bool
			}{
				{"", true}, {fmt.Sprint(task.ID), true}, {fmt.Sprintf("%q", fmt.Sprint(task.ID)), true},
				{fmt.Sprintf("\"00%d\"", task.ID), true},
				{fmt.Sprint(task.ID + 1), false}, {fmt.Sprintf("%q", fmt.Sprint(task.ID+1)), false},
				{"0", false}, {"-1", false}, {"132.5", false}, {"true", false}, {"null", false},
				{"[]", false}, {"{}", false}, {`"invalid"`, false}, {"9223372036854775808", false},
			}
			for _, input := range inputs {
				t.Run(fmt.Sprintf("%s/cross=%v/params=%s/id=%s", tool.Name(), exposeCross, spec.params, input.id), func(t *testing.T) {
					body := spec.params
					if input.id != "" {
						if body != "" {
							body += ","
						}
						body += `"task_id":` + input.id
					}
					raw := json.RawMessage("{" + body + "}")
					prepared, err := agent.PrepareToolInput(tool, raw)
					if err != nil {
						if input.allowed {
							t.Fatal(err)
						}
						return
					}
					r, err := tool.Call(t.Context(), prepared, nil)
					if err != nil {
						t.Fatal(err)
					}
					failed := r.IsError || strings.Contains(r.Flatten(), `"error"`)
					if failed == input.allowed {
						t.Fatalf("allowed=%v: %s", input.allowed, r.Flatten())
					}
					if input.allowed && !strings.Contains(r.Flatten(), "SELECTOR_FINDING") {
						t.Fatalf("lost current task result: %s", r.Flatten())
					}
				})
			}
		}
	}
}

func TestContextCatalogHasNoRetiredEntrypoints(t *testing.T) {
	for _, seed := range agent.BuiltinToolSeeds() {
		if _, retired := db.RetiredToolReplacements[seed.Key]; retired {
			t.Fatal(seed.Key)
		}
	}
	s := &Server{}
	for _, tool := range s.orchestrationTools() {
		if _, retired := db.RetiredToolReplacements[tool.Name()]; retired {
			t.Fatal(tool.Name())
		}
	}
}
