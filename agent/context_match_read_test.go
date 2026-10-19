package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/neouks/series/db"
)

func TestContextMatchReadRoundTrip(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("match read", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	text := strings.Repeat("中文🙂", 5000) + "Needle_%'" + strings.Repeat("尾", 300)
	node, err := es.AddNode(db.KindFact, map[string]any{"summary": "brief", "detail": text}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	compound, err := es.AddNode(db.KindFact, map[string]any{"summary": "compound", "evidence": map[string]any{"request": text}}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	array, err := es.AddNode(db.KindFact, map[string]any{"summary": "array", "evidence": []any{text}}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	compoundFields := map[int64]string{compound: "/evidence/request", array: "/evidence/0"}
	intent, err := es.AddIntent(map[string]any{"summary": "work"}, 1, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = es.AppendActivity(db.Activity{NodeID: &intent, Worker: "worker", Kind: "tool_result", Summary: "brief", Detail: text})
	if err != nil {
		t.Fatal(err)
	}
	finding, err := es.AddNode(db.KindFinding, map[string]any{"summary": "report-only hit"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`INSERT INTO findings(node_id,task_id,name,severity,vulnclass,report) VALUES($1,$2,'match report','high','test',$3)`, finding, task.ID, text); err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(es, "test")
	ts.SetTaskID(task.ID)
	for _, types := range [][]string{{"fact"}, {"finding"}, {"activity"}, {"fact", "finding", "activity"}} {
		raw, _ := json.Marshal(map[string]any{"types": types, "q": "needle_%'"})
		r, err := ts.contextSearch().Call(t.Context(), raw, nil)
		if err != nil || r.IsError {
			t.Fatal(r.Flatten(), err)
		}
		var out struct{ Items []db.ContextHit }
		if err = json.Unmarshal([]byte(r.Flatten()), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) == 0 {
			t.Fatal("no hits")
		}
		for _, hit := range out.Items {
			if compoundFields[hit.ID] != "" && hit.Kind == "fact" {
				if hit.MatchOffset != nil {
					t.Fatal("compound offset invented")
				}
			} else if hit.MatchOffset == nil || *hit.MatchOffset != 15000 {
				t.Fatalf("bad Unicode offset %+v", hit)
			}
			if hit.Read == nil {
				t.Fatal("missing read")
			}
			in, _ := json.Marshal(hit.Read)
			got, err := ts.contextGet().Call(t.Context(), in, nil)
			if err != nil || got.IsError {
				t.Fatal(got.Flatten(), err)
			}
			if compoundFields[hit.ID] != "" && hit.Kind == "fact" {
				if !strings.Contains(got.Flatten(), compoundFields[hit.ID]) {
					t.Fatal(got.Flatten())
				}
			} else if !strings.Contains(got.Flatten(), "Needle_%'") {
				t.Fatal("read missed hit", got.Flatten())
			}
		}
	}
	// Search permission does not create a detail grant.
	ctx := WithContextAccess(t.Context(), ContextAccess{Local: []string{"fact"}})
	r, _ := ts.contextSearch().Call(ctx, json.RawMessage(`{"types":["fact"],"q":"needle"}`), nil)
	if strings.Contains(r.Flatten(), `"read":`) {
		t.Fatal("ungranted detail advertised")
	}
	// Inherited references remain within the child's local scope.
	child, err := d.CreateTaskWithOptions("child", "goal", db.TaskCreateOptions{SourceTaskIDs: []int64{task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(child.ID)
	ct := NewToolSet(d.Exploration(child.ExplorationID), "test")
	ct.SetTaskID(child.ID)
	ctx = WithContextAccess(t.Context(), ContextAccess{Local: []string{"fact", "detail"}})
	r, _ = ct.contextSearch().Call(ctx, json.RawMessage(fmt.Sprintf(`{"task_id":%d,"types":["fact"],"q":"needle"}`, child.ID)), nil)
	var inherited struct{ Items []db.ContextHit }
	json.Unmarshal([]byte(r.Flatten()), &inherited)
	found := false
	for _, h := range inherited.Items {
		if h.ID == node {
			found = true
			in, _ := json.Marshal(h.Read)
			got, e := ct.contextGet().Call(ctx, in, nil)
			if e != nil || got.IsError || !strings.Contains(got.Flatten(), "Needle_%'") {
				t.Fatal("inherited read failed", string(in), got.Flatten(), e)
			}
		}
	}
	if !found {
		t.Fatal("inherited hit missing", r.Flatten())
	}
}
