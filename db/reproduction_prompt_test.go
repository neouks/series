package db

import (
	"os"
	"testing"
)

func TestReproductionPromptUpgradePreservesCustomAndHistory(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, err := d.GetAgentByKey("reporter")
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		a, err = d.CreateAgent("reporter", "reporter", "test")
		if err != nil {
			t.Fatal(err)
		}
		defer d.DeleteAgent("reporter")
	}
	var original *int64
	var maxID int64
	d.QueryRow(`SELECT current_prompt_id FROM agents WHERE id=$1`, a.ID).Scan(&original)
	d.QueryRow(`SELECT COALESCE(max(id),0) FROM agent_prompts WHERE agent_id=$1`, a.ID).Scan(&maxID)
	defer func() {
		d.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, original, a.ID)
		d.Exec(`DELETE FROM agent_prompts WHERE agent_id=$1 AND id>$2`, a.ID, maxID)
	}()
	for _, key := range []string{"finding_reproduction_prompts_v1", "finding_reproduction_prompts_report_v1"} {
		value, found, err := d.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if found {
				d.SetSetting(key, value)
			} else {
				d.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		}()
	}
	old, err := os.ReadFile("testdata/reporter-before-reproduction.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, custom := range []bool{false, true} {
		text := string(old)
		if custom {
			text = "用户定制：" + text
		}
		if _, err = d.ResetPromptToDefault(a.ID, text); err != nil {
			t.Fatal(err)
		}
		d.Exec(`DELETE FROM settings WHERE key='finding_reproduction_prompts_v1'`)
		for range 2 {
			if err = d.MigrateFindingReproductionPrompts("NEW REPRODUCTION DEFAULT"); err != nil {
				t.Fatal(err)
			}
		}
		got, err := d.CurrentPrompt(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := "NEW REPRODUCTION DEFAULT"
		if custom {
			want = text
		}
		if got != want {
			t.Fatal("default/custom upgrade incorrect")
		}
		var count int
		d.QueryRow(`SELECT count(*) FROM agent_prompts WHERE agent_id=$1 AND template_text=$2`, a.ID, text).Scan(&count)
		if count != 1 {
			t.Fatal("prior prompt rewritten", count)
		}
	}
}
