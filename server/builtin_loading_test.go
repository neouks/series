package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestBuiltinLiveRevocationAndLoadingPreview(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	oldResolve, oldFinding := agent.ToolResolve, agent.FindingTrafficBindingEnabled
	t.Cleanup(func() { agent.ToolResolve, agent.FindingTrafficBindingEnabled = oldResolve, oldFinding })
	wireTools(pg, nil)
	search, err := pg.GetTool(agent.BuiltinSearchName)
	if err != nil || search == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agents, _ := json.Marshal(search.Agents)
		if err := pg.UpdateTool(search.Key, search.Description, search.Schema, agents, search.Enabled); err != nil {
			t.Error(err)
		}
	})
	searchAgents := append([]string(nil), search.Agents...)
	if !contains(searchAgents, "worker") {
		searchAgents = append(searchAgents, "worker")
	}
	agents, _ := json.Marshal(searchAgents)
	if err := pg.UpdateTool(search.Key, search.Description, search.Schema, agents, true); err != nil {
		t.Fatal(err)
	}
	const name = "builtin_loading_fixture"
	t.Cleanup(func() {
		pg.Exec(`DELETE FROM tools WHERE key=$1`, name)
		pg.Exec(`DELETE FROM tool_usage WHERE tool_key=$1`, name)
	})
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	if err := pg.SeedTool(name, "fixture", schema, json.RawMessage(`["worker"]`)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	base := actool.Build(actool.Spec{Name: name, Schema: map[string]any{"type": "object", "properties": map[string]any{}}, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		calls++
		return actool.Text("done"), nil
	}})
	resolved := names(resolveToolsForTest(t, t.Context(), "worker", []actool.CoreTool{base}))[name]
	invoke := func(wantErr bool) {
		t.Helper()
		r, e := resolved.Call(t.Context(), json.RawMessage(`{}`), nil)
		if e != nil || r.IsError != wantErr {
			t.Fatalf("call error=%v result=%+v", e, r)
		}
	}
	invoke(false)
	// A running instance must also observe per-role feature revocation.
	role, err := pg.GetAgentByKey("worker")
	if err != nil {
		t.Fatal(err)
	}
	if role == nil {
		role, err = pg.CreateAgent("worker", "worker", "test fixture")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pg.DeleteAgent("worker") })
	} else {
		t.Cleanup(func() { pg.SetAgentWebSearch("worker", role.WebSearch) })
	}
	oldSearch, hadSearch, err := pg.GetSetting(settingWebSearchOn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadSearch {
			pg.SetSetting(settingWebSearchOn, oldSearch)
		} else {
			pg.Exec(`DELETE FROM settings WHERE key=$1`, settingWebSearchOn)
		}
	})
	if err := pg.SetSetting(settingWebSearchOn, "true"); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetAgentWebSearch("worker", true); err != nil {
		t.Fatal(err)
	}
	searchCalls := 0
	web := actool.Build(actool.Spec{Name: "web_search", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		searchCalls++
		return actool.Text("fixture search"), nil
	}})
	webTools, err := configureBuiltinLoading(t.Context(), pg, "worker", []actool.CoreTool{web}, mustToolRows(t, pg))
	if err != nil || names(webTools)["web_search"] == nil {
		t.Fatalf("web search not assembled: %v", err)
	}
	web = names(webTools)["web_search"]
	if r, err := web.Call(t.Context(), json.RawMessage(`{}`), nil); err != nil || r.IsError {
		t.Fatalf("enabled search failed: %v %+v", err, r)
	}
	if err := pg.SetAgentWebSearch("worker", false); err != nil {
		t.Fatal(err)
	}
	if r, err := web.Call(t.Context(), json.RawMessage(`{}`), nil); err != nil || !r.IsError || searchCalls != 1 {
		t.Fatalf("revoked search executed: %v %+v calls=%d", err, r, searchCalls)
	}
	if err := pg.UpdateTool(name, "fixture", schema, json.RawMessage(`["planner"]`), true); err != nil {
		t.Fatal(err)
	}
	invoke(true)
	if err := pg.UpdateTool(name, "fixture", schema, json.RawMessage(`["worker"]`), false); err != nil {
		t.Fatal(err)
	}
	invoke(true)
	if calls != 1 {
		t.Fatal("revoked implementation executed")
	}
	if err := pg.UpdateTool(name, "fixture", schema, json.RawMessage(`["worker"]`), true); err != nil {
		t.Fatal(err)
	}
	invoke(false)
	s := &Server{m: &Manager{pg: pg}}
	preview := func() string {
		t.Helper()
		w := httptest.NewRecorder()
		s.pgListTools(w, httptest.NewRequest("GET", "/api/tools", nil))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var response struct {
			Tools []*db.Tool `json:"tools"`
		}
		json.Unmarshal(w.Body.Bytes(), &response)
		for _, tool := range response.Tools {
			if tool.Key == name {
				if tool.SchemaBytes == 0 {
					t.Fatal("schema size missing")
				}
				return tool.Loading["worker"]
			}
		}
		t.Fatal("missing tool")
		return ""
	}
	previous, exists, err := pg.GetSetting(agent.BuiltinLoadingSetting)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if exists {
			pg.SetSetting(agent.BuiltinLoadingSetting, previous)
		} else {
			pg.Exec(`DELETE FROM settings WHERE key=$1`, agent.BuiltinLoadingSetting)
		}
	})
	pg.SetSetting(agent.BuiltinLoadingSetting, "on_demand")
	if preview() != "on_demand" {
		t.Fatal("missing on-demand preview")
	}
	pg.SetSetting(agent.BuiltinLoadingSetting, "direct")
	if preview() != "resident" {
		t.Fatal("missing direct preview")
	}
	pg.SetSetting(agent.BuiltinLoadingSetting, "on_demand")
	if err := pg.UpdateTool(search.Key, search.Description, search.Schema, agents, false); err != nil {
		t.Fatal(err)
	}
	fallback := names(resolveToolsForTest(t, t.Context(), "worker", []actool.CoreTool{base}))
	if fallback[agent.BuiltinSearchName] != nil || fallback[name] == nil || preview() != "resident" {
		t.Fatal("disabled search did not fall back to direct")
	}
	invalid := httptest.NewRecorder()
	s.putSettings(invalid, httptest.NewRequest("PUT", "/api/settings", strings.NewReader(`{"builtin_tool_loading":"invalid"}`)))
	if invalid.Code != 400 {
		t.Fatal("invalid loading mode accepted")
	}
}
