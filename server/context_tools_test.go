package server

import (
	"encoding/json"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
	"slices"
	"testing"
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
