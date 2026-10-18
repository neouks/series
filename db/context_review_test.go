package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func TestContextReadRejectsUnverifiableDigest(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	task, e := d.CreateTask("digest review", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	empty, e := s.AddNode(KindDigest, map[string]any{"body": "private unverifiable digest"}, 0, "active", "test", nil)
	if e != nil {
		t.Fatal(e)
	}
	child, e := s.AddNode(KindFact, map[string]any{"summary": "child of empty digest"}, 0, "confirmed", "test", nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Link(empty, RelDerivedFrom, child); e != nil {
		t.Fatal(e)
	}
	access, e := d.Assets().TaskNodeAccess(task.ID, []int64{empty, child})
	if e != nil {
		t.Fatal(e)
	}
	for _, id := range []int64{empty, child} {
		if access[id].CanRead {
			t.Fatal("fixture unexpectedly readable", id)
		}
		if row, e := s.ContextNode(t.Context(), id, "", 0, 0, 1000); !errors.Is(e, sql.ErrNoRows) {
			t.Fatalf("unverified digest lineage exposed: %v %v", row, e)
		}
	}
	hits, e := s.SearchContext(t.Context(), ContextSearch{Types: []string{"digest", "fact"}, Limit: 10, Upper: 1 << 62})
	if e != nil {
		t.Fatal(e)
	}
	for _, hit := range hits {
		if hit.ID == empty || hit.ID == child {
			t.Fatal("search exposed unverified lineage", hit)
		}
	}
	// Worker policy intentionally retains its existing treatment of empty digests.
	if _, e = s.WithWorkerRead().ContextNode(t.Context(), empty, "", 0, 0, 1000); e != nil {
		t.Fatal(e)
	}
}

func TestExplicitContextBindingOnFreshCatalog(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	tx, e := d.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`DELETE FROM settings WHERE key=$1`, ContextCapabilitiesSetting); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`INSERT INTO tools(key,system,description,schema,agents,enabled) VALUES('context_search',true,'test','{}','[]',true) ON CONFLICT(key) DO UPDATE SET system=true,agents='[]',enabled=true`); e != nil {
		t.Fatal(e)
	}
	if e = updateContextBindingGrants(tx, "context_search", json.RawMessage(`["worker"]`), true); e != nil {
		t.Fatal(e)
	}
	var raw []byte
	if e = tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, ContextCapabilitiesSetting).Scan(&raw); e != nil {
		t.Fatal("fresh catalog lost explicit binding", e)
	}
	var grants map[string]ContextToolGrants
	if e = json.Unmarshal(raw, &grants); e != nil {
		t.Fatal(e)
	}
	has := func(v []string, w string) bool {
		for _, s := range v {
			if s == w {
				return true
			}
		}
		return false
	}
	if !has(grants["worker"].Local, "fact") || !has(grants["worker"].Local, "detail") || len(grants["worker"].Cross) != 0 {
		t.Fatal("new grant lost defaults or enlarged cross-task access", grants)
	}
	// Enabling a binding that never had search grants must make it usable.
	if _, e = tx.Exec(`UPDATE tools SET agents='["disabled-role"]',enabled=false WHERE key='context_search'`); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`UPDATE settings SET value='{"disabled-role":{"local":["detail"],"cross":[]}}' WHERE key=$1`, ContextCapabilitiesSetting); e != nil {
		t.Fatal(e)
	}
	if e = updateContextBindingGrants(tx, "context_search", json.RawMessage(`["disabled-role"]`), true); e != nil {
		t.Fatal(e)
	}
	if e = tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, ContextCapabilitiesSetting).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(raw, &grants)
	if !has(grants["disabled-role"].Local, "fact") {
		t.Fatal("enabling tool did not grant its capabilities", grants)
	}
	// A temporary disable must not widen a migrated fact-only binding.
	if _, e = tx.Exec(`UPDATE settings SET value='{"disabled-role":{"local":["fact"],"cross":[]}}' WHERE key=$1`, ContextCapabilitiesSetting); e != nil {
		t.Fatal(e)
	}
	if e = updateContextBindingGrants(tx, "context_search", json.RawMessage(`["disabled-role"]`), true); e != nil {
		t.Fatal(e)
	}
	if e = tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, ContextCapabilitiesSetting).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(raw, &grants)
	if has(grants["disabled-role"].Local, "finding") {
		t.Fatal("toggle widened migrated permission", grants)
	}
}

func TestContextProjectionPreservesLargeJSONIntegers(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	exp, e := d.CreateExploration("exact payload", "goal")
	if e != nil {
		t.Fatal(e)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	id, e := s.AddNode(KindFact, map[string]any{"detail": map[string]any{"business_id": int64(9007199254740993)}}, 0, "confirmed", "test", nil)
	if e != nil {
		t.Fatal(e)
	}
	row, e := s.ContextNode(t.Context(), id, "/detail/business_id", 0, 0, 1000)
	if e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(row["payload"].(map[string]any)["value"])
	if e != nil || string(b) != "9007199254740993" {
		t.Fatalf("original evidence number changed: %s %v", b, e)
	}
}

func TestContextReadsRejectDeletedTask(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	task, e := d.CreateTask("deleted context", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { d.Exec(`UPDATE tasks SET deleted_at=NULL WHERE id=$1`, task.ID); d.DeleteTask(task.ID) }()
	s := d.Exploration(task.ExplorationID)
	id, e := s.AddNode(KindFact, map[string]any{"summary": "must disappear"}, 0, "confirmed", "test", nil)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, task.ID); e != nil {
		t.Fatal(e)
	}
	for _, store := range []*ExplorationStore{s, s.WithWorkerRead()} {
		if _, e = store.ContextNode(t.Context(), id, "", 0, 0, 1000); !errors.Is(e, sql.ErrNoRows) {
			t.Fatal("deleted task was treated as unscoped exploration", e)
		}
	}
}

func TestContextOutputUsesLiveAuthorization(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	task, e := d.CreateTaskWithOptions("output access", "goal", TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	as, es := d.Assets(), d.Exploration(task.ExplorationID)
	a, e := as.UpsertRootDomain(UpsertRootDomainReq{Domain: "context-output.review.test", TaskID: task.ID, AgentDiscovered: true})
	if e != nil {
		t.Fatal(e)
	}
	id, e := es.AddNode(KindIntent, map[string]any{"summary": "output fixture"}, 1, "running", "test", []int64{a})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = es.AppendActivity(Activity{NodeID: &id, Kind: "text", Detail: "progress evidence"}); e != nil {
		t.Fatal(e)
	}
	if _, e = es.ContextOutput(t.Context(), id, 0, 100); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("strict output read allowed pending", e)
	}
	out, e := es.WithWorkerRead().ContextOutput(t.Context(), id, 0, 100)
	if e != nil || out["text"] != "progress evidence" || out["is_final"] != false {
		t.Fatal(out, e)
	}
	if e = as.BlockTaskAssets(task.ID, []int64{a}, "user", ""); e != nil {
		t.Fatal(e)
	}
	if _, e = es.WithWorkerRead().ContextOutput(t.Context(), id, 0, 100); !errors.Is(e, sql.ErrNoRows) {
		t.Fatal("blocked output readable", e)
	}
}
