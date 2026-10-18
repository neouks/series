package server

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestContextDocumentationMatchesRoleViews(t *testing.T) {
	ts := agent.NewToolSet(nil, "test")
	tool := applyContextPolicy(agent.ResolveBuiltinTool(ts.ContextGetTool(), "", map[string]any{}), agent.ContextAccess{Local: []string{"output"}})
	item := tool.InputSchema()["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)
	if !slices.Contains(item["required"].([]any), any("view")) {
		t.Fatal("output-only reader told it may omit view")
	}
	view := item["properties"].(map[string]any)["view"].(map[string]any)
	desc := view["description"].(string)
	if strings.Contains(desc, "members") || strings.Contains(desc, "默认detail") || !strings.Contains(desc, "output") {
		t.Fatal(desc)
	}
	result, err := tool.Call(t.Context(), json.RawMessage(`{"items":[{"ref":"node:1"}]}`), nil)
	if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "view") {
		t.Fatal(result, err)
	}
	search := applyContextPolicy(ts.ContextSearchTool(), agent.ContextAccess{Local: []string{"activity_all"}})
	types := search.InputSchema()["properties"].(map[string]any)["types"].(map[string]any)
	if !slices.Equal(types["items"].(map[string]any)["enum"].([]string), []string{"activity"}) {
		t.Fatal(types)
	}
}

func TestHostToolDocumentationHasNoRetiredEntrypoints(t *testing.T) {
	s := &Server{}
	for _, tool := range append(s.orchestrationTools(), s.platformTools()...) {
		raw, _ := json.Marshal(tool.InputSchema())
		for old := range db.RetiredToolReplacements {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(old) + `\b`).MatchString(tool.Description() + string(raw)) {
				t.Errorf("%s advertises %s", tool.Name(), old)
			}
		}
	}
}

func TestBuiltinSchemaRefreshPreservesConfiguredDefaultsAndBindings(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	const flag = "tool_schema_refresh_v9_contract_docs"
	flagValue, flagFound, err := pg.GetSetting(flag)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if flagFound {
			pg.SetSetting(flag, flagValue)
		} else {
			pg.Exec(`DELETE FROM settings WHERE key=$1`, flag)
		}
	})
	if _, err = pg.Exec(`DELETE FROM settings WHERE key=$1`, flag); err != nil {
		t.Fatal(err)
	}
	// Back up catalog rows touched by the real migration; tests leave no edits behind.
	rows, err := pg.Query(`SELECT to_jsonb(t) FROM tools t WHERE system`)
	if err != nil {
		t.Fatal(err)
	}
	var backup [][]byte
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		backup = append(backup, b)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	t.Cleanup(func() {
		pg.Exec(`DELETE FROM tools WHERE system`)
		for _, b := range backup {
			if _, err := pg.Exec(`INSERT INTO tools SELECT * FROM jsonb_populate_record(NULL::tools,$1::jsonb)`, string(b)); err != nil {
				t.Error(err)
			}
		}
	})
	// Stale schema from before mixed-search support; a valid page-size preference survives.
	schema := json.RawMessage(`{"type":"object","properties":{"types":{"type":"array","description":"activity 需单独查询"},"limit":{"type":"integer","default":20,"description":"上限1000"},"old_param":{"type":"string"}}}`)
	if err = pg.UpsertToolForce("context_search", "旧版 list_facts", schema, json.RawMessage(`["worker"]`)); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.Exec(`UPDATE tools SET enabled=false WHERE key='context_search'`); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: pg}}
	s.refreshBuiltinToolSchemas()
	got, err := pg.GetTool("context_search")
	if err != nil || got == nil {
		t.Fatal(err)
	}
	if got.Enabled || !slices.Equal(got.Agents, []string{"worker"}) {
		t.Fatal("binding changed", got)
	}
	if strings.Contains(got.Description, "list_facts") || strings.Contains(string(got.Schema), "activity 需单独查询") || strings.Contains(string(got.Schema), "old_param") {
		t.Fatal("stale instructions remain", got)
	}
	var current map[string]any
	if err = json.Unmarshal(got.Schema, &current); err != nil {
		t.Fatal(err)
	}
	limit := current["properties"].(map[string]any)["limit"].(map[string]any)
	if limit["default"] != float64(20) || limit["maximum"] != float64(50) {
		t.Fatal(limit)
	}
	value, _, err := pg.GetSetting(flag)
	if err != nil || value != "true" {
		t.Fatal(value, err)
	}
	before := string(got.Schema)
	s.refreshBuiltinToolSchemas()
	got, err = pg.GetTool("context_search")
	if err != nil || string(got.Schema) != before {
		t.Fatal("non-idempotent refresh", err)
	}
}
