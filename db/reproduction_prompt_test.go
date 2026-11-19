package db

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
)

func TestReproductionPromptUpgradePreservesCustomAndHistory(t *testing.T) {
	testReportPromptUpgrade(t, "reporter-before-reproduction.txt", "finding_reproduction_prompts_v1", "finding_reproduction_prompts_report_v1", (*DB).MigrateFindingReproductionPrompts)
}

func TestInlineHTTPPromptUpgradePreservesCustomAndHistory(t *testing.T) {
	testReportPromptUpgrade(t, "reporter-before-inline-http.txt", "finding_report_inline_http_v1", "finding_report_inline_http_migration_v1", (*DB).MigrateInlineHTTPReportPrompt)
}

func TestInlineHTTPPromptPreviousDefaultHash(t *testing.T) {
	previous, err := os.ReadFile("testdata/reporter-before-inline-http.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(previous)); got != "0f33f6646416207cfcaab1205702ee63e594f5b8b71ee8a3e0b4b163202b0a5a" {
		t.Fatal(got)
	}
}

func testReportPromptUpgrade(t *testing.T, fixture, flag, reportKey string, migrate func(*DB, string) error) {
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
	for _, key := range []string{flag, reportKey} {
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
	old, err := os.ReadFile("testdata/" + fixture)
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
		d.Exec(`DELETE FROM settings WHERE key=$1`, flag)
		for range 2 {
			if err = migrate(d, "NEW REPRODUCTION DEFAULT"); err != nil {
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
