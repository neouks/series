package db

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestPlannerGraphReadMigrationBoundOnly(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, key := range []string{"context_search", "graph_overview"} {
		var saved []byte
		if err = d.QueryRow(`SELECT COALESCE((SELECT to_jsonb(t) FROM tools t WHERE key=$1),'null')`, key).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			d.Exec(`DELETE FROM tools WHERE key=$1`, key)
			if string(saved) != "null" {
				d.Exec(`INSERT INTO tools SELECT * FROM jsonb_populate_record(NULL::tools,$1::jsonb)`, string(saved))
			}
		})
	}
	for _, key := range []string{ContextCapabilitiesSetting, plannerGraphReadsMigration} {
		v, found, e := d.GetSetting(key)
		if e != nil {
			t.Fatal(e)
		}
		defer func() {
			if found {
				d.SetSetting(key, v)
			} else {
				d.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		}()
	}
	const original = `{"planner":{"local":["intent","overview"],"cross":["finding"]},"worker":{"local":["fact"],"cross":[]}}`
	for _, mode := range []string{"enabled", "disabled", "unbound", "custom", "no overview grant", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			for _, key := range []string{"context_search", "graph_overview"} {
				if e := d.UpsertToolForce(key, "custom prompt", json.RawMessage(`{}`), json.RawMessage(`["planner"]`)); e != nil {
					t.Fatal(e)
				}
			}
			d.Exec(`DELETE FROM settings WHERE key=$1`, plannerGraphReadsMigration)
			d.SetSetting(ContextCapabilitiesSetting, original)
			switch mode {
			case "disabled":
				d.Exec(`UPDATE tools SET enabled=false WHERE key='context_search'`)
			case "unbound":
				d.Exec(`UPDATE tools SET agents='[]' WHERE key='graph_overview'`)
			case "custom":
				d.Exec(`UPDATE tools SET system=false WHERE key='graph_overview'`)
			case "no overview grant":
				d.SetSetting(ContextCapabilitiesSetting, `{"planner":{"local":["intent"],"cross":["finding"]}}`)
			case "fresh":
				d.Exec(`DELETE FROM settings WHERE key=$1`, ContextCapabilitiesSetting)
			}
			before, found, _ := d.GetSetting(ContextCapabilitiesSetting)
			if err := d.MigratePlannerGraphReads(); err != nil {
				t.Fatal(err)
			}
			after, foundAfter, _ := d.GetSetting(ContextCapabilitiesSetting)
			if mode != "enabled" && (before != after || found != foundAfter) {
				t.Fatal("ineligible config changed", mode, after)
			}
			if mode == "enabled" {
				var got map[string]ContextToolGrants
				json.Unmarshal([]byte(after), &got)
				want := map[string]ContextToolGrants{"planner": {Local: []string{"intent", "overview", "hint", "digest"}, Cross: []string{"finding"}}, "worker": {Local: []string{"fact"}, Cross: []string{}}}
				if !reflect.DeepEqual(got, want) {
					t.Fatal(got)
				}
			}
			d.SetSetting(ContextCapabilitiesSetting, original)
			if err := d.MigratePlannerGraphReads(); err != nil {
				t.Fatal(err)
			}
			if got, _, _ := d.GetSetting(ContextCapabilitiesSetting); got != original {
				t.Fatal("repeated migration changed grants")
			}
			for _, key := range []string{"context_search", "graph_overview"} {
				tool, e := d.GetTool(key)
				if e != nil || tool.Description != "custom prompt" {
					t.Fatal("custom description changed", e)
				}
			}
		})
	}
}

func TestPlannerOnDemandPromptUpgradeAfterPriorMigration(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	a, e := d.GetAgentByKey("planner")
	if e != nil || a == nil {
		t.Fatal(e)
	}
	var original *int64
	var maxID int64
	if e = d.QueryRow(`SELECT current_prompt_id FROM agents WHERE id=$1`, a.ID).Scan(&original); e != nil {
		t.Fatal(e)
	}
	if e = d.QueryRow(`SELECT COALESCE(max(id),0) FROM agent_prompts WHERE agent_id=$1`, a.ID).Scan(&maxID); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		d.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, original, a.ID)
		d.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1 AND id>$2`, a.ID, maxID)
	})
	for _, key := range []string{"tool_prompt_contracts_v2", "planner_on_demand_prompt_v1", "planner_on_demand_prompt_report_v1"} {
		v, found, e := d.GetSetting(key)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if found {
				d.SetSetting(key, v)
			} else {
				d.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		})
	}
	d.SetSetting("tool_prompt_contracts_v2", "true")
	old := "known default with preloaded graph"
	for _, custom := range []bool{false, true} {
		d.Exec(`DELETE FROM settings WHERE key='planner_on_demand_prompt_v1'`)
		text := old
		if custom {
			text = "custom " + old
		}
		if _, e = d.SavePrompt(a.ID, text, "fixture", "test"); e != nil {
			t.Fatal(e)
		}
		if e = d.MigratePlannerOnDemandPrompt("on demand default", []string{fmt.Sprintf("%x", sha256.Sum256([]byte(old)))}); e != nil {
			t.Fatal(e)
		}
		got, e := d.CurrentPrompt(a.ID)
		if e != nil {
			t.Fatal(e)
		}
		want := "on demand default"
		if custom {
			want = text
		}
		if got != want {
			t.Fatal(got)
		}
		if e = d.MigratePlannerOnDemandPrompt("must not run twice", []string{fmt.Sprintf("%x", sha256.Sum256([]byte(got)))}); e != nil {
			t.Fatal(e)
		}
		if repeated, _ := d.CurrentPrompt(a.ID); repeated != want {
			t.Fatal("not idempotent", repeated)
		}
	}
}

func TestPlannerProactivePromptUpgradeAfterPriorMigrations(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	a, e := d.GetAgentByKey("planner")
	if e != nil || a == nil {
		t.Fatal(e)
	}
	var original *int64
	var maxID int64
	if e = d.QueryRow(`SELECT current_prompt_id FROM agents WHERE id=$1`, a.ID).Scan(&original); e != nil {
		t.Fatal(e)
	}
	if e = d.QueryRow(`SELECT COALESCE(max(id),0) FROM agent_prompts WHERE agent_id=$1`, a.ID).Scan(&maxID); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		d.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, original, a.ID)
		d.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1 AND id>$2`, a.ID, maxID)
	})
	for _, key := range []string{"tool_prompt_contracts_v2", "planner_on_demand_prompt_v1", "planner_proactive_prompt_v1", "planner_proactive_prompt_report_v1"} {
		v, found, e := d.GetSetting(key)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if found {
				d.SetSetting(key, v)
			} else {
				d.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		})
	}
	d.SetSetting("tool_prompt_contracts_v2", "true")
	d.SetSetting("planner_on_demand_prompt_v1", "true")
	old := "known default with forced query gate"
	for _, custom := range []bool{false, true} {
		d.Exec(`DELETE FROM settings WHERE key='planner_proactive_prompt_v1'`)
		text := old
		if custom {
			text = "custom " + old
		}
		if _, e = d.SavePrompt(a.ID, text, "fixture", "test"); e != nil {
			t.Fatal(e)
		}
		if e = d.MigratePlannerProactivePrompt("proactive default", []string{fmt.Sprintf("%x", sha256.Sum256([]byte(old)))}); e != nil {
			t.Fatal(e)
		}
		got, e := d.CurrentPrompt(a.ID)
		if e != nil {
			t.Fatal(e)
		}
		want := "proactive default"
		if custom {
			want = text
		}
		if got != want {
			t.Fatal(got)
		}
		if e = d.MigratePlannerProactivePrompt("must not run twice", []string{fmt.Sprintf("%x", sha256.Sum256([]byte(got)))}); e != nil {
			t.Fatal(e)
		}
		if repeated, _ := d.CurrentPrompt(a.ID); repeated != want {
			t.Fatal("not idempotent", repeated)
		}
	}
}
