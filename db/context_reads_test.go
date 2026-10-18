package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestContextReadsFullTextAndWindows(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exp, err := d.CreateExploration("context fixture", "goal")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	body := strings.Repeat("中文🙂", 5000) + "Needle_%'" + strings.Repeat("尾部", 500)
	id, err := s.AddNode(KindFact, map[string]any{"summary": "摘要无关键词", "detail": body}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	upper, err := s.ContextUpper(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchContext(context.Background(), ContextSearch{Types: []string{"fact"}, Query: "needle_%'", Limit: 10, Upper: upper})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != id || hits[0].Field != "/detail" || !strings.Contains(hits[0].Snippet, "Needle_%'") {
		t.Fatalf("hits: %+v", hits)
	}
	var joined strings.Builder
	for offset := 0; ; {
		row, e := s.ContextNode(context.Background(), id, "/detail", 0, offset, 137)
		if e != nil {
			t.Fatal(e)
		}
		p := row["payload"].(map[string]any)
		joined.WriteString(p["value"].(string))
		if p["next_offset"] == nil {
			break
		}
		next, e := p["next_offset"].(json.Number).Int64()
		if e != nil {
			t.Fatal(e)
		}
		offset = int(next)
	}
	if joined.String() != body {
		t.Fatal("body continuation lost text")
	}
	root, err := s.ContextNode(context.Background(), id, "", 0, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(root)
	if len(encoded) > 2000 {
		t.Fatalf("projection loaded body: %d", len(encoded))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.SearchContext(ctx, ContextSearch{Types: []string{"fact"}, Limit: 10, Upper: upper}); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestContextActivityAndReportSearch(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	task, e := d.CreateTask("context sources", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	intent, e := s.AddIntent(map[string]any{"summary": "work"}, 1, nil, "test")
	if e != nil {
		t.Fatal(e)
	}
	for _, a := range []Activity{{NodeID: &intent, Worker: "worker", Kind: "text", Summary: "summary NEEDLE", Detail: "different body"}, {NodeID: &intent, Worker: "worker", Kind: "tool_result", Summary: "short", Detail: strings.Repeat("x", 10000) + "body NEEDLE"}, {NodeID: &intent, Worker: "worker", Kind: "thinking", Detail: "private NEEDLE"}, {Worker: "main", Kind: "text", Detail: "main NEEDLE"}} {
		if _, e = s.AppendActivity(a); e != nil {
			t.Fatal(e)
		}
	}
	hits, e := s.SearchContext(t.Context(), ContextSearch{Types: []string{"activity"}, Query: "needle", Limit: 10, Upper: 1 << 62})
	if e != nil {
		t.Fatal(e)
	}
	if len(hits) != 2 || hits[0].Field != "/detail" || hits[1].Field != "/summary" {
		t.Fatal(hits)
	}
	for _, h := range hits {
		if !strings.Contains(h.Snippet, "NEEDLE") {
			t.Fatal(h)
		}
	}
	activityID := hits[0].ID
	node, e := s.AddNode(KindFinding, map[string]any{"summary": "nothing"}, 0, "confirmed", "test", nil)
	if e != nil {
		t.Fatal(e)
	}
	var findingID int64
	if e = d.QueryRow(`INSERT INTO findings(node_id,task_id,name,severity,vulnclass,report) VALUES($1,$2,'report fixture','high','test','OnlyReportNEEDLE') RETURNING id`, node, task.ID).Scan(&findingID); e != nil {
		t.Fatal(e)
	}
	hits, e = s.SearchContext(t.Context(), ContextSearch{Types: []string{"finding"}, Query: "onlyreportneedle", Limit: 10, Upper: 1 << 62})
	if e != nil {
		t.Fatal(e)
	}
	if len(hits) != 1 || hits[0].FindingID != findingID || hits[0].FindingNodeID != node || hits[0].Field != "/report" {
		t.Fatal(hits)
	}
	// Separate task must neither search nor read an unrelated node.
	other, e := d.CreateTask("unrelated", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(other.ID)
	os := d.Exploration(other.ExplorationID)
	if _, e = os.ContextNode(t.Context(), node, "", 0, 0, 1000); e == nil {
		t.Fatal("cross-task read")
	}
	if _, e = d.Exec(`INSERT INTO task_relations(task_id,source_task_id) VALUES($1,$2)`, other.ID, task.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = os.ContextNode(t.Context(), node, "/report", 0, 0, 1000); e != nil {
		t.Fatal("direct source unavailable", e)
	}
	if _, e = os.ContextActivity(t.Context(), activityID, 0, 1000); e == nil {
		t.Fatal("running source intent readable")
	}
	if e = s.SetIntentState(intent, "done"); e != nil {
		t.Fatal(e)
	}
	if _, e = os.ContextActivity(t.Context(), activityID, 0, 1000); e != nil {
		t.Fatal(e)
	}
}
