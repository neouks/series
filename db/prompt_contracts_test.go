package db

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestPromptContractUpgradeAfterCompletedRename(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	for _, key := range []string{"tool_context_prompts_v1", "tool_prompt_contracts_v2", "tool_prompt_contracts_report_v2"} {
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
		if _, err = d.Exec(`DELETE FROM settings WHERE key=$1`, key); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.SetSetting("tool_context_prompts_v1", "true"); err != nil {
		t.Fatal(err)
	}
	defaults, hashes := map[string]string{}, map[string][]string{}
	ids := map[string]int64{}
	versions := map[string]int{}
	old := "old default uses update_skill and node_detail"
	for _, role := range []string{"worker", "planner"} {
		a, err := d.GetAgentByKey(role)
		if err != nil || a == nil {
			t.Fatal(role, err)
		}
		ids[role] = a.ID
		var original *int64
		var maxID int64
		if err = d.QueryRow(`SELECT current_prompt_id FROM agents WHERE id=$1`, a.ID).Scan(&original); err != nil {
			t.Fatal(err)
		}
		if err = d.QueryRow(`SELECT COALESCE(max(id),0) FROM agent_prompts WHERE agent_id=$1`, a.ID).Scan(&maxID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			d.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, original, a.ID)
			d.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1 AND id>$2`, a.ID, maxID)
		})
		text := old
		if role == "planner" {
			text = "custom " + old
		}
		if versions[role], err = d.SavePrompt(a.ID, text, "fixture", "test"); err != nil {
			t.Fatal(err)
		}
		defaults[role] = "current context_get and update_skill_file"
		hashes[role] = []string{"unmatched", fmt.Sprintf("%x", sha256.Sum256([]byte(old)))}
	}
	if err = d.MigrateBuiltinPromptContracts(defaults, hashes); err != nil {
		t.Fatal(err)
	}
	for role, id := range ids {
		got, err := d.CurrentPrompt(id)
		if err != nil {
			t.Fatal(err)
		}
		want := defaults[role]
		if role == "planner" {
			want = "custom " + old
		}
		if got != want {
			t.Fatalf("%s: %q", role, got)
		}
	}
	report, _, err := d.GetSetting("tool_prompt_contracts_report_v2")
	if err != nil || !strings.Contains(report, "custom_preserved") || !strings.Contains(report, "update_skill_file") || !strings.Contains(report, "node_detail") {
		t.Fatal(report, err)
	}
	var before, after int
	if err = d.QueryRow(`SELECT count(*) FROM agent_prompts`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err = d.MigrateBuiltinPromptContracts(defaults, hashes); err != nil {
		t.Fatal(err)
	}
	if err = d.QueryRow(`SELECT count(*) FROM agent_prompts`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("non-idempotent migration")
	}
	var kept int
	if err = d.QueryRow(`SELECT count(*) FROM agent_prompts WHERE agent_id=$1 AND template_text=$2 AND version=$3`, ids["worker"], old, versions["worker"]).Scan(&kept); err != nil || kept != 1 {
		t.Fatal("old audit version changed", kept, err)
	}
}

func TestPromptReportParameterWarningDoesNotConfuseIDNamespaces(t *testing.T) {
	for _, text := range []string{"get_finding_traffic(finding_id=123); update_finding_report(finding_node_id=456, report=body)", "update_finding_report(finding_node_id, report); finding_id 是证据 ID"} {
		if legacyFindingReportParameter.MatchString(text) {
			t.Fatal("correct prompt flagged", text)
		}
	}
	for _, text := range []string{"update_finding_report(finding_id=123, report=body)", "update_finding_report(\nreport=text, finding_id=id)", "update_finding_report(finding_id, report)"} {
		if !legacyFindingReportParameter.MatchString(text) {
			t.Fatal("legacy parameter missed", text)
		}
	}
}
