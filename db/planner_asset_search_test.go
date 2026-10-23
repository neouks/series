package db

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

func TestPlannerAssetSearchMigrationAndBinding(t *testing.T) {
	testPlannerContextReadMigrationAndBinding(t, "asset_search", "assets", plannerAssetSearchMigration, (*DB).MigratePlannerAssetSearch)
}

func TestPlannerIntentSearchMigrationAndBinding(t *testing.T) {
	if p := DefaultContextGrants("planner"); !slices.Contains(p.Local, "intent") || slices.Contains(p.Cross, "intent") {
		t.Fatal("Planner must have task-local intent search by default", p)
	}
	testPlannerContextReadMigrationAndBinding(t, "context_search", "intent", plannerIntentSearchMigration, (*DB).MigratePlannerIntentSearch)
}

func testPlannerContextReadMigrationAndBinding(t *testing.T, toolKey, capability, flag string, migrate func(*DB) error) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	var original []byte
	if err := d.QueryRow(`SELECT COALESCE((SELECT to_jsonb(t) FROM tools t WHERE key=$1),'null')`, toolKey).Scan(&original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM tools WHERE key=$1`, toolKey)
		if string(original) != "null" {
			d.Exec(`INSERT INTO tools SELECT * FROM jsonb_populate_record(NULL::tools,$1::jsonb)`, string(original))
		}
	})
	for _, key := range []string{ContextCapabilitiesSetting, flag} {
		value, found, err := d.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if found {
				d.SetSetting(key, value)
			} else {
				d.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		})
	}
	const before = `{"planner":{"local":["untested","fact"],"cross":["finding"]},"worker":{"local":["untested","fact"],"cross":[]}}`
	for _, spec := range []struct {
		name                           string
		enabled, system, bound, policy bool
	}{
		{"legacy", true, true, true, true}, {"disabled", false, true, true, true},
		{"unbound", true, true, false, true}, {"custom", true, false, true, true}, {"fresh", true, true, true, false},
	} {
		t.Run(spec.name, func(t *testing.T) {
			if _, err := d.Exec(`DELETE FROM settings WHERE key=ANY($1::text[])`, []string{ContextCapabilitiesSetting, flag}); err != nil {
				t.Fatal(err)
			}
			if spec.policy {
				if err := d.SetSetting(ContextCapabilitiesSetting, before); err != nil {
					t.Fatal(err)
				}
			}
			agents := json.RawMessage(`["worker"]`)
			if spec.bound {
				agents = json.RawMessage(`["planner","worker"]`)
			}
			if err := d.UpsertToolForce(toolKey, "custom description", json.RawMessage(`{"limit":10}`), agents); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(`UPDATE tools SET enabled=$1,system=$2 WHERE key=$3`, spec.enabled, spec.system, toolKey); err != nil {
				t.Fatal(err)
			}
			if err := migrate(d); err != nil {
				t.Fatal(err)
			}
			raw, found, err := d.GetSetting(ContextCapabilitiesSetting)
			if err != nil {
				t.Fatal(err)
			}
			if found != spec.policy {
				t.Fatal("changed default-only policy persistence")
			}
			if spec.policy {
				var grants, want map[string]ContextToolGrants
				if err := json.Unmarshal([]byte(raw), &grants); err != nil {
					t.Fatal(err)
				}
				json.Unmarshal([]byte(before), &want)
				if spec.enabled && spec.system && spec.bound {
					p := want["planner"]
					p.Local = append(p.Local, capability)
					want["planner"] = p
				}
				if !reflect.DeepEqual(grants, want) {
					t.Fatal("unrelated capability changed", grants, want)
				}
			}
			row, err := d.GetTool(toolKey)
			if err != nil || row == nil || row.Enabled != spec.enabled || row.System != spec.system || string(mustMarshalPlannerTest(row.Agents)) != string(agents) || row.Description != "custom description" {
				t.Fatal(row, err)
			}
			// Idempotent repair does not repeatedly grant a deliberately removed capability.
			if err := d.SetSetting(ContextCapabilitiesSetting, before); err != nil {
				t.Fatal(err)
			}
			if err := migrate(d); err != nil {
				t.Fatal(err)
			}
			if got, _, _ := d.GetSetting(ContextCapabilitiesSetting); got != before {
				t.Fatal("migration ran twice")
			}
			// Saving/re-enabling the explicit Planner binding repairs the same contract.
			if err := d.UpdateTool(toolKey, row.Description, row.Schema, agents, true); err != nil {
				t.Fatal(err)
			}
			got, _, _ := d.GetSetting(ContextCapabilitiesSetting)
			var grants map[string]ContextToolGrants
			json.Unmarshal([]byte(got), &grants)
			if slices.Contains(grants["planner"].Local, capability) != (spec.system && spec.bound) {
				t.Fatal("binding repair mismatch", grants)
			}
			if slices.Contains(grants["worker"].Local, capability) || !slices.Equal(grants["planner"].Cross, []string{"finding"}) {
				t.Fatal("binding enlarged other permissions", grants)
			}
		})
	}
	// Invalid persisted policy must fail atomically and remain retryable.
	d.Exec(`DELETE FROM settings WHERE key=$1`, flag)
	d.Exec(`UPDATE tools SET system=true,enabled=true,agents='["planner"]' WHERE key=$1`, toolKey)
	d.SetSetting(ContextCapabilitiesSetting, `{invalid`)
	if err := migrate(d); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if _, found, _ := d.GetSetting(flag); found {
		t.Fatal("failed repair marked complete")
	}
	d.SetSetting(ContextCapabilitiesSetting, before)
	if err := migrate(d); err != nil {
		t.Fatal(err)
	}
}

func mustMarshalPlannerTest(v any) []byte { b, _ := json.Marshal(v); return b }
