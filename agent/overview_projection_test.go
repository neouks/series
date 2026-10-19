package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOverviewProjectionFairSections(t *testing.T) {
	data := map[string]any{"task": map[string]any{"description": "fixture", "goal": "goal"}, "frontier_open": 1000, "facts": 1000, "findings_total": 1000, "unavailable": map[string]any{"hints": "database query failed"}, "partial": true}
	for _, key := range []string{"running_intents", "open_intents", "goals", "recent_done_intents", "recent_facts", "finding_list", "cold_digests"} {
		rows := []any{}
		for i := 0; i < 80; i++ {
			rows = append(rows, map[string]any{"id": i + 1, "summary": strings.Repeat("证", 350), "body": strings.Repeat("huge", 1500)})
		}
		data[key] = rows
	}
	before, _ := json.Marshal(data)
	out := boundedGraphOverview(data, 24000, 0)
	raw, _ := json.Marshal(out)
	if utf8.RuneCount(raw) > 24000 {
		t.Fatal("budget", utf8.RuneCount(raw))
	}
	for _, k := range []string{"task", "goals", "running_intents", "open_intents", "recent_facts", "finding_list", "cold_digests"} {
		if out[k] == nil {
			t.Fatal("starved section", k)
		}
		if rows, ok := out[k].([]any); ok && len(rows) == 0 {
			t.Fatal("no first record", k)
		}
	}
	sections := out["sections"].(map[string]any)
	if sections["hints"].(map[string]any)["unavailable"] != true {
		t.Fatal("failed query became empty")
	}
	for _, k := range []string{"open_intents", "recent_facts", "finding_list"} {
		m := sections[k].(map[string]any)
		if m["has_more"] != true || m["read"] == nil {
			t.Fatal(k, m)
		}
	}
	row := out["cold_digests"].([]any)[0].(map[string]any)
	fields := row["omitted_fields"].(map[string]any)
	if fields["body"] == nil || row["body"] != nil {
		t.Fatal("long field not deferred", row)
	}
	after, _ := json.Marshal(data)
	if string(before) != string(after) {
		t.Fatal("source mutated")
	}
	prompt := renderGraphOverview(data)
	if utf8.RuneCountInString(prompt) > 24000 {
		t.Fatal("prompt wrappers exceed budget")
	}
	old := renderLightTaggedOrdered("graph_overview", data, []string{"task", "goals", "open_intents"}, 24000)
	t.Logf("overview old_chars=%d new_chars=%d old_bytes=%d new_bytes=%d", utf8.RuneCountInString(old), utf8.RuneCountInString(prompt), len(old), len(prompt))
}
func TestOverviewProjectionScopeAndTinyBudget(t *testing.T) {
	data := map[string]any{"related_tasks": []any{map[string]any{"source_task_id": 999, "inherited": true, "recent_facts": []any{map[string]any{"id": 7, "summary": strings.Repeat("x", 5000)}}}}, "cold_digests": []any{map[string]any{"id": 7, "body": strings.Repeat("x", 5000)}}}
	raw, _ := json.Marshal(boundedGraphOverview(data, 24000, 30000))
	if strings.Contains(string(raw), `"task_id":999`) {
		t.Fatal("invented cross-task grant")
	}
	small := boundedGraphOverview(data, 200, 200)
	if small["context_unavailable"] != true {
		t.Fatal(fmt.Sprint(small))
	}
}

func TestOverviewReadScopeUsesRequestedTask(t *testing.T) {
	data := map[string]any{"cold_digests": []any{map[string]any{"id": 7, "body": strings.Repeat("z", 2000), "source_task_id": 999}}}
	out := boundedGraphOverview(data, 24000, 30000, "132")
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"task_id":"132"`) || strings.Contains(string(raw), `"task_id":999`) {
		t.Fatal(string(raw))
	}
	if len(raw) > 30000 {
		t.Fatal("scope metadata exceeds budget")
	}
}
