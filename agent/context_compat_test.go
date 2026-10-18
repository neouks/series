package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func TestContextGetItemBudgetAndContinuation(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("item budgets", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	body := strings.Repeat("中文🙂\n", 2000)
	id, err := es.AddNode(db.KindFact, map[string]any{"summary": "budget", "detail": body}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(es, "worker")
	ts.SetTaskID(task.ID)
	tool := DecorateTool(ts.contextGet(), "", nil)
	ctx := WithContextAccess(t.Context(), ContextAccess{Local: []string{"detail"}})
	call := func(input string, outputLimit int) map[string]any {
		t.Helper()
		r, e := tool.Call(ctx, json.RawMessage(input), &actool.ToolContext{MaxOutputChars: outputLimit})
		if e != nil || r.IsError {
			t.Fatalf("%s: %s %v", input, r.Flatten(), e)
		}
		if len(r.Flatten()) > outputLimit {
			t.Fatal("output budget exceeded")
		}
		var out map[string]any
		if e := json.Unmarshal([]byte(r.Flatten()), &out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	for _, top := range []string{"", `,"max_chars":16000`} {
		out := call(fmt.Sprintf(`{"items":[{"max_chars":12000,"ref":"node:%d","view":"detail"}]%s}`, id, top), 30000)
		if out["items"].([]any)[0].(map[string]any)["error"] != nil {
			t.Fatal(out)
		}
	}
	input := fmt.Sprintf(`{"items":[{"ref":"node:%d","field":"/detail","max_chars":73}],"max_chars":200}`, id)
	var joined strings.Builder
	for {
		out := call(input, 4000)
		row := out["items"].([]any)[0].(map[string]any)
		text := row["payload"].(map[string]any)["value"].(string)
		if len([]rune(text)) > 73 {
			t.Fatal("per-item cap exceeded")
		}
		joined.WriteString(text)
		if row["next"] == nil {
			break
		}
		next, _ := json.Marshal(row["next"])
		if !strings.Contains(string(next), `"max_chars":73`) {
			t.Fatal("continuation lost cap", string(next))
		}
		input = string(next)
	}
	if joined.String() != body {
		t.Fatal("continuation changed original content")
	}
	intent, err := es.AddIntent(map[string]any{"summary": "budgeted parents"}, 1, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Link(id, db.RelDerivedFrom, intent); err != nil {
		t.Fatal(err)
	}
	parents := call(fmt.Sprintf(`{"items":[{"ref":"node:%d","view":"parents","max_chars":2}]}`, intent), 4000)
	parent := parents["items"].([]any)[0].(map[string]any)["parents"].([]any)[0].(map[string]any)
	if len([]rune(parent["summary"].(string))) > 2 {
		t.Fatal("parent projection exceeded cap", parent)
	}
	out := call(fmt.Sprintf(`{"items":[{"ref":"node:%d","field":"/detail","max_chars":24000},{"ref":"node:%d","field":"/detail","max_chars":24000}],"max_chars":100}`, id, id), 30000)
	total := 0
	for _, item := range out["items"].([]any) {
		total += len([]rune(item.(map[string]any)["payload"].(map[string]any)["value"].(string)))
	}
	if total > 100 {
		t.Fatal("item budgets bypassed total", total)
	}
	out = call(fmt.Sprintf(`{"items":[{"ref":"node:%d","field":"/detail","max_chars":24000},{"ref":"node:%d","field":"/detail","max_chars":24000}],"max_chars":1}`, id, id), 30000)
	items := out["items"].([]any)
	if len([]rune(items[0].(map[string]any)["payload"].(map[string]any)["value"].(string))) != 1 || items[1].(map[string]any)["error"] == nil {
		t.Fatal("tiny total budget multiplied by batch size", out)
	}
	for _, bad := range []string{"0", "-1", "24001", "1.5", "null", `"100"`} {
		r, err := tool.Call(ctx, json.RawMessage(fmt.Sprintf(`{"items":[{"ref":"node:%d","max_chars":%s}]}`, id, bad)), nil)
		if err != nil || !r.IsError {
			t.Fatalf("invalid cap %s: %s %v", bad, r.Flatten(), err)
		}
	}
	r, err := tool.Call(WithContextAccess(t.Context(), ContextAccess{}), json.RawMessage(fmt.Sprintf(`{"items":[{"ref":"node:%d","max_chars":100}]}`, id)), nil)
	if err != nil || strings.Contains(r.Flatten(), "budget") || !strings.Contains(r.Flatten(), "读取权限") {
		t.Fatal("cap bypassed permission", r.Flatten(), err)
	}
}

func TestContextMixedSearchPermissionsAndCursor(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("mixed search", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	intent, err := es.AddIntent(map[string]any{"summary": "work"}, 1, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, kind := range []string{db.KindFact, db.KindFinding} {
		id, err := es.AddNode(kind, map[string]any{"summary": "short", "detail": "中文 AcTuAtOr evidence"}, 0, "confirmed", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		want[fmt.Sprintf("node:%d", id)] = true
	}
	aid, err := es.AppendActivity(db.Activity{NodeID: &intent, Kind: "tool_result", Detail: "中文 AcTuAtOr output"})
	if err != nil {
		t.Fatal(err)
	}
	want[fmt.Sprintf("activity:%d", aid)] = true
	for _, a := range []db.Activity{{NodeID: &intent, Kind: "thinking", Detail: "actuator private"}, {Kind: "text", Worker: "main", Detail: "actuator main"}} {
		if _, err := es.AppendActivity(a); err != nil {
			t.Fatal(err)
		}
	}
	ts := NewToolSet(es, "worker")
	ts.SetTaskID(task.ID)
	tool := DecorateTool(ts.contextSearch(), "", nil)
	ctx := WithContextAccess(t.Context(), ContextAccess{Local: []string{"fact", "finding", "activity_all"}})
	q := map[string]any{"q": "actuator", "types": []string{"fact", "finding", "activity"}, "limit": 1}
	seen := map[string]bool{}
	firstCursor := ""
	for page := 0; page < 10; page++ {
		in, _ := json.Marshal(q)
		r, err := tool.Call(ctx, in, &actool.ToolContext{MaxOutputChars: 4000})
		if err != nil || r.IsError || len(r.Flatten()) > 4000 {
			t.Fatal(r.Flatten(), err)
		}
		var out struct {
			Items   []db.ContextHit `json:"items"`
			HasMore bool            `json:"has_more"`
			Next    string          `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(r.Flatten()), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) > 1 {
			t.Fatal("limit applied per type instead of per request")
		}
		for _, h := range out.Items {
			if !want[h.Ref] || seen[h.Ref] || !strings.Contains(h.Snippet, "AcTuAtOr") {
				t.Fatalf("invalid/duplicate hit: %+v", h)
			}
			seen[h.Ref] = true
		}
		if !out.HasMore {
			break
		}
		if page == 0 {
			firstCursor = out.Next
			// Even backdated new records cannot enter a previously opened search.
			id, err := es.AddNode(db.KindFact, map[string]any{"summary": "actuator new"}, 0, "confirmed", "test", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(`UPDATE exploration_nodes SET created_at=created_at-interval '1 day' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			id, err = es.AppendActivity(db.Activity{NodeID: &intent, Kind: "text", Detail: "actuator new"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(`UPDATE activity SET created_at=created_at-interval '1 day' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
		q["cursor"] = out.Next
	}
	if len(seen) != len(want) {
		t.Fatal("missing mixed results", seen, want)
	}
	for _, p := range []ContextAccess{{Local: []string{"fact", "finding"}}, {Local: []string{"activity_all"}}, {Cross: []string{"fact", "finding", "activity_all"}}} {
		in, _ := json.Marshal(q)
		r, err := tool.Call(WithContextAccess(t.Context(), p), in, nil)
		if err != nil || !r.IsError {
			t.Fatal("mixed query bypassed resource/local permission", r.Flatten(), err)
		}
	}
	for _, change := range []map[string]any{{"q": "different"}, {"types": []string{"activity"}}, {"task_id": task.ID + 1}} {
		q["cursor"] = firstCursor
		copy := map[string]any{}
		for k, v := range q {
			copy[k] = v
		}
		for k, v := range change {
			copy[k] = v
		}
		in, _ := json.Marshal(copy)
		r, err := tool.Call(ctx, in, nil)
		if err != nil || !r.IsError {
			t.Fatal("changed query accepted cursor", r.Flatten(), err)
		}
	}
}
