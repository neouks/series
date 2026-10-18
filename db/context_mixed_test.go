package db

import (
	"context"
	"fmt"
	"testing"
)

func TestMixedContextLiveAuthorizationAndInheritance(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTaskWithOptions("mixed authorization", "goal", TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	as, es := d.Assets(), d.Exploration(task.ExplorationID)
	aid, err := as.UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("mixed-%d.test", task.ID), TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := es.AddNode(KindIntent, map[string]any{"summary": "work"}, 1, "running", "test", []int64{aid})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{KindFact, KindFinding} {
		id, err := es.AddNode(kind, map[string]any{"summary": "actuator evidence"}, 0, "confirmed", "test", []int64{aid})
		if err != nil {
			t.Fatal(err)
		}
		if err := es.Link(intent, RelYields, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := es.AppendActivity(Activity{NodeID: &intent, Kind: "tool_result", Detail: "actuator output"}); err != nil {
		t.Fatal(err)
	}
	q := ContextSearch{Types: []string{"fact", "finding", "activity"}, Query: "actuator", Limit: 10, AssetID: aid, IntentID: intent}
	check := func(s *ExplorationStore, n int) {
		t.Helper()
		hits, _, err := s.SearchMixedContext(t.Context(), q, nil)
		if err != nil || len(hits) != n {
			t.Fatalf("want=%d hits=%+v err=%v", n, hits, err)
		}
	}
	check(es, 0)
	check(es.WithWorkerRead(), 3)
	if err := as.ApproveTaskAssets(task.ID, []int64{aid}, "test", ""); err != nil {
		t.Fatal(err)
	}
	check(es, 3)
	other, err := d.CreateTask("mixed unrelated", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(other.ID)
	os := d.Exploration(other.ExplorationID)
	check(os, 0)
	if _, err := d.Exec(`INSERT INTO task_relations(task_id,source_task_id) VALUES($1,$2)`, other.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	// Existing inherited asset policy requires approval in the consuming task too.
	if _, err := as.AttachAssetsToTask(other.ID, []int64{aid}, "test scope"); err != nil {
		t.Fatal(err)
	}
	check(os, 2) // live source Worker transcript stays private
	if err := es.SetIntentState(intent, "done"); err != nil {
		t.Fatal(err)
	}
	check(os, 3)
	q.Limit = 1
	_, next, err := es.SearchMixedContext(t.Context(), q, nil)
	if err != nil || next == nil {
		t.Fatal(next, err)
	}
	if err := as.RevokeTaskAssets(task.ID, []int64{aid}, "test", ""); err != nil {
		t.Fatal(err)
	}
	if hits, _, err := es.SearchMixedContext(t.Context(), q, next); err != nil || len(hits) != 0 {
		t.Fatal("revoked cursor read", hits, err)
	}
	check(es.WithWorkerRead(), 0)
	check(os, 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := es.SearchMixedContext(ctx, q, nil); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestMixedContextEqualTimeAndIDPagination(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTask("mixed ties", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	intent, err := es.AddIntent(map[string]any{"summary": "work"}, 1, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := d.QueryRow(`SELECT greatest((SELECT coalesce(max(id),0) FROM exploration_nodes),(SELECT coalesce(max(id),0) FROM activity))+1000000`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO exploration_nodes(id,exploration_id,kind,state,payload,created_at) VALUES($1,$2,'fact','confirmed','{"summary":"UNIQUE_TIE_MARKER"}','2026-01-01T00:00:00Z')`, id, task.ExplorationID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO activity(id,exploration_id,node_id,kind,detail,created_at) VALUES($1,$2,$3,'text','UNIQUE_TIE_MARKER','2026-01-01T00:00:00Z')`, id, task.ExplorationID, intent); err != nil {
		t.Fatal(err)
	}
	q := ContextSearch{Types: []string{"fact", "activity"}, Query: "UNIQUE_TIE_MARKER", Limit: 1}
	hits, next, err := es.SearchMixedContext(t.Context(), q, nil)
	if err != nil || len(hits) != 2 || next == nil || hits[0].Ref != fmt.Sprintf("node:%d", id) {
		t.Fatal(hits, next, err)
	}
	hits, next, err = es.SearchMixedContext(t.Context(), q, next)
	if err != nil || len(hits) != 1 || next != nil || hits[0].Ref != fmt.Sprintf("activity:%d", id) {
		t.Fatal(hits, next, err)
	}
}
