package db_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestPlannerDirectDispatchPromptMigration(t *testing.T) {
	dsn := os.Getenv("SERIES_PG_DSN")
	if dsn == "" {
		t.Skip("requires explicit isolated SERIES_PG_DSN")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	a, err := d.GetAgentByKey("planner")
	if err != nil || a == nil {
		t.Fatal(a, err)
	}
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
	for _, key := range []string{"planner_proactive_prompt_v1", "planner_direct_dispatch_prompt_v1", "planner_direct_dispatch_prompt_report_v1"} {
		value, found, e := d.GetSetting(key)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if found {
				d.SetSetting(key, value)
			} else {
				d.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		})
	}
	old, err := os.ReadFile("testdata/planner-before-direct-dispatch.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(agent.PreviousPlannerDirectDispatchPromptHashes, fmt.Sprintf("%x", sha256.Sum256(old))) {
		t.Fatal("missing shipped default hash")
	}
	defaults := agent.BuiltinPromptSeeds()
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom=%v", custom), func(t *testing.T) {
			d.Exec(`DELETE FROM settings WHERE key IN('planner_direct_dispatch_prompt_v1','planner_direct_dispatch_prompt_report_v1')`)
			if err = d.SetSetting("planner_proactive_prompt_v1", "true"); err != nil {
				t.Fatal(err)
			}
			text := string(old)
			if custom {
				text = "custom task policy\n" + text
			}
			if _, err = d.SavePrompt(a.ID, text, "fixture", "test"); err != nil {
				t.Fatal(err)
			}
			if err = d.MigratePlannerDirectDispatchPrompt(defaults["planner"], agent.PreviousPlannerDirectDispatchPromptHashes); err != nil {
				t.Fatal(err)
			}
			got, e := d.CurrentPrompt(a.ID)
			if e != nil {
				t.Fatal(e)
			}
			want := defaults["planner"]
			if custom {
				want = text
			}
			if got != want {
				t.Fatal("wrong prompt migration", custom)
			}
			var preserved string
			if err = d.QueryRow(`SELECT template_text FROM agent_prompts WHERE agent_id=$1 AND template_text=$2 LIMIT 1`, a.ID, text).Scan(&preserved); err != nil || preserved != text {
				t.Fatal("previous prompt version changed", err)
			}
			if err = d.MigratePlannerDirectDispatchPrompt("must not repeat", []string{fmt.Sprintf("%x", sha256.Sum256([]byte(want)))}); err != nil {
				t.Fatal(err)
			}
			if repeated, e := d.CurrentPrompt(a.ID); e != nil || repeated != want {
				t.Fatal("migration not idempotent", e)
			}
		})
	}
}
