package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"testing"
)

func TestContextCatalogMigrationPreservesGrants(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	keys := []string{"graph_overview", "traffic_get"}
	for old, targets := range RetiredToolReplacements {
		keys = append(keys, old)
		keys = append(keys, targets...)
	}
	rows, e := d.Query(`SELECT to_jsonb(t) FROM tools t WHERE key=ANY($1::text[])`, keys)
	if e != nil {
		t.Fatal(e)
	}
	var saved [][]byte
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			t.Fatal(e)
		}
		saved = append(saved, b)
	}
	rows.Close()
	defer func() {
		d.Exec(`DELETE FROM tools WHERE key=ANY($1::text[])`, keys)
		for _, b := range saved {
			if _, e := d.Exec(`INSERT INTO tools SELECT * FROM jsonb_populate_record(NULL::tools,$1::jsonb)`, string(b)); e != nil {
				t.Error(e)
			}
		}
	}()
	for _, key := range []string{"tool_context_catalog_v1", ContextCapabilitiesSetting, plannerAssetSearchMigration} {
		v, ok, e := d.GetSetting(key)
		if e != nil {
			t.Fatal(e)
		}
		defer func(key, v string, ok bool) {
			if ok {
				d.SetSetting(key, v)
			} else {
				d.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		}(key, v, ok)
		d.Exec(`DELETE FROM settings WHERE key=$1`, key)
	}
	d.Exec(`DELETE FROM tools WHERE key=ANY($1::text[])`, keys)
	seed := func(key, bindings string) {
		t.Helper()
		if e := d.SeedTool(key, "fixture", json.RawMessage(`{}`), json.RawMessage(bindings)); e != nil {
			t.Fatal(e)
		}
	}
	for _, key := range []string{"context_search", "context_get", "asset_search", "asset_get", "graph_overview", "traffic_get"} {
		seed(key, `["default-must-not-leak"]`)
	}
	seed("list_facts", `["reader"]`)
	seed("node_detail", `["reader"]`)
	seed("list_task_findings", `["remote-reader"]`)
	seed("list_assets", `["asset-reader"]`)
	seed("list_untested_assets", `["planner","coverage-reader"]`)
	seed("get_worker_trace", `["reader"]`)
	d.Exec(`UPDATE tools SET enabled=false WHERE key='get_worker_trace'`)
	if _, e = d.Exec(`UPDATE tools SET system=false WHERE key='context_search'`); e != nil {
		t.Fatal(e)
	}
	if e = d.MigrateContextTools(); e == nil {
		t.Fatal("custom name collision ignored")
	}
	if old, _ := d.GetTool("list_facts"); old == nil {
		t.Fatal("failed migration was not atomic")
	}
	if _, e = d.Exec(`UPDATE tools SET system=true WHERE key='context_search'`); e != nil {
		t.Fatal(e)
	}
	if e = d.MigrateContextTools(); e != nil {
		t.Fatal(e)
	}
	raw, _, e := d.GetSetting(ContextCapabilitiesSetting)
	if e != nil {
		t.Fatal(e)
	}
	var grants map[string]ContextToolGrants
	json.Unmarshal([]byte(raw), &grants)
	if slices.Contains(grants["planner"].Local, "assets") || !slices.Contains(grants["planner"].Local, "untested") {
		t.Fatal("fixture did not reproduce legacy untested-only Planner", grants)
	}
	if e = d.MigratePlannerAssetSearch(); e != nil {
		t.Fatal(e)
	}
	raw, _, e = d.GetSetting(ContextCapabilitiesSetting)
	if e != nil {
		t.Fatal(e)
	}
	json.Unmarshal([]byte(raw), &grants)
	if !slices.Contains(grants["planner"].Local, "assets") || !slices.Contains(grants["planner"].Local, "untested") || len(grants["planner"].Cross) != 0 || slices.Contains(grants["coverage-reader"].Local, "assets") {
		t.Fatal("Planner repair changed unrelated grants", grants)
	}
	if !slices.Contains(grants["reader"].Local, "fact") || !slices.Contains(grants["reader"].Local, "detail") || slices.Contains(grants["reader"].Local, "activity") {
		t.Fatal(grants)
	}
	if len(grants["remote-reader"].Local) > 0 || !slices.Contains(grants["remote-reader"].Cross, "finding") {
		t.Fatal(grants)
	}
	tool, e := d.GetTool("context_search")
	if e != nil || tool == nil || slices.Contains(tool.Agents, "default-must-not-leak") {
		t.Fatal(tool, e)
	}
	if old, _ := d.GetTool("list_facts"); old != nil {
		t.Fatal("retired tool still registered")
	}
	if e = d.UpdateTool("context_search", "edited", tool.Schema, json.RawMessage(`["reader","new-reader"]`), true); e != nil {
		t.Fatal(e)
	}
	raw, _, _ = d.GetSetting(ContextCapabilitiesSetting)
	json.Unmarshal([]byte(raw), &grants)
	if slices.Contains(grants["reader"].Local, "finding") || !slices.Contains(grants["new-reader"].Local, "finding") {
		t.Fatal("explicit rebind or prose edit grants wrong", grants)
	}
	if e = d.MigrateContextTools(); e != nil {
		t.Fatal(e)
	}
	again, _ := d.GetTool("context_search")
	if !slices.Contains(again.Agents, "new-reader") {
		t.Fatal("reran migration")
	}
	if CanonicalToolUsageKey("get_worker_trace") != "context_get" || CanonicalToolUsageKey("list_assets") != "asset_search" {
		t.Fatal("history mapping")
	}
}

func TestContextPromptMigrationPreservesCustomText(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	for _, key := range []string{"tool_context_prompts_v1", "tool_context_prompt_migration_report"} {
		v, ok, e := d.GetSetting(key)
		if e != nil {
			t.Fatal(e)
		}
		defer func(key, v string, ok bool) {
			if ok {
				d.SetSetting(key, v)
			} else {
				d.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		}(key, v, ok)
		d.Exec(`DELETE FROM settings WHERE key=$1`, key)
	}
	// Use existing built-ins, restore current pointers and remove only our added versions.
	defaults := map[string]string{}
	hashes := map[string]string{}
	for _, key := range []string{"worker", "planner"} {
		a, e := d.GetAgentByKey(key)
		if e != nil || a == nil {
			t.Fatal(e)
		}
		var old *int64
		var maxID int64
		d.QueryRow(`SELECT current_prompt_id FROM agents WHERE id=$1`, a.ID).Scan(&old)
		d.QueryRow(`SELECT COALESCE(max(id),0) FROM agent_prompts WHERE agent_id=$1`, a.ID).Scan(&maxID)
		defer func(id int64, old *int64, maxID int64) {
			d.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, old, id)
			d.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1 AND id>$2`, id, maxID)
		}(a.ID, old, maxID)
		body := "default uses list_facts"
		if key == "planner" {
			body = "user custom uses list_facts"
		}
		if _, e = d.SavePrompt(a.ID, body, "test", "test"); e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256([]byte("default uses list_facts"))
		hashes[key] = hex.EncodeToString(sum[:])
		defaults[key] = "new context_search"
	}
	if e = d.MigrateContextPrompts(defaults, hashes); e != nil {
		t.Fatal(e)
	}
	worker, _ := d.GetAgentByKey("worker")
	text, _ := d.CurrentPrompt(worker.ID)
	if text != "new context_search" {
		t.Fatal(text)
	}
	planner, _ := d.GetAgentByKey("planner")
	text, _ = d.CurrentPrompt(planner.ID)
	if text != "user custom uses list_facts" {
		t.Fatal("custom overwritten", text)
	}
	raw, _, _ := d.GetSetting("tool_context_prompt_migration_report")
	var report map[string]any
	json.Unmarshal([]byte(raw), &report)
	if report["planner"] == nil {
		t.Fatal("missing custom migration notice")
	}
	if e = d.MigrateContextPrompts(defaults, hashes); e != nil {
		t.Fatal(e)
	}
}

func TestContextUsageMappingDoesNotRewriteLedger(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	before, e := d.ToolUsageCounts()
	if e != nil {
		t.Fatal(e)
	}
	ids := []int64{}
	defer func() { d.Exec(`DELETE FROM tool_usage WHERE id=ANY($1::bigint[])`, ids) }()
	for _, key := range []string{"list_facts", "list_findings", "context_search"} {
		var id int64
		if e = d.QueryRow(`INSERT INTO tool_usage(tool_key,agent_key) VALUES($1,'context-test') RETURNING id`, key).Scan(&id); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	after, e := d.ToolUsageCounts()
	if e != nil {
		t.Fatal(e)
	}
	if after["context_search"]-before["context_search"] != 3 {
		t.Fatal(before, after)
	}
	var originals int
	if e = d.QueryRow(`SELECT count(*) FROM tool_usage WHERE id=ANY($1::bigint[]) AND tool_key IN ('list_facts','list_findings')`, ids).Scan(&originals); e != nil || originals != 2 {
		t.Fatal("ledger rewritten", originals, e)
	}
}
