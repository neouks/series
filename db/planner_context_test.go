package db

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestPlannerBootstrapScopedCountsAndEventPriority(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	source, err := d.CreateTask("source", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(source.ID)
	task, err := d.CreateTaskWithOptions("bootstrap", "goal", TaskCreateOptions{SourceTaskIDs: []int64{source.ID}, AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	other, err := d.CreateTask("other", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(other.ID)
	s := d.Exploration(task.ExplorationID)
	add := func(store *ExplorationStore, kind, state, summary string, assets ...int64) int64 {
		t.Helper()
		id, e := store.AddNode(kind, map[string]any{"summary": summary, "detail": strings.Repeat("DO_NOT_PRELOAD", 1000)}, 1, state, "test", assets)
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	edge := func(a, b int64, rel string) {
		t.Helper()
		if _, e := d.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,dst_id,rel) VALUES($1,$2,$3,$4)`, s.ID(), a, b, rel); e != nil {
			t.Fatal(e)
		}
	}
	pending, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("bootstrap-%d.test", task.ID), TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	restricted := add(s, KindFact, "confirmed", "pending-secret", pending)
	child := add(s, KindFact, "confirmed", "ancestor-secret")
	edge(restricted, child, "derived_from")
	old := add(s, KindIntent, "running", "old-running")
	for _, kind := range []string{KindFact, KindFinding, KindGoal, KindHint} {
		state := "confirmed"
		if kind == KindGoal {
			state = "open"
		}
		if kind == KindHint {
			state = "active"
		}
		add(s, kind, state, kind)
	}
	digest := add(s, KindDigest, "active", "digest-secret")
	edge(digest, restricted, "covers")
	sourceNode := add(d.Exploration(source.ExplorationID), KindFact, "confirmed", "inherited-public")
	outside := add(d.Exploration(other.ExplorationID), KindFact, "confirmed", "outside-secret")
	if _, err = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) SELECT $1,'intent','open',jsonb_build_object('summary','queued '||n) FROM generate_series(1,330)n`, s.ID()); err != nil {
		t.Fatal(err)
	}
	parent := add(s, KindFact, "confirmed", "parent")
	event := add(s, KindIntent, "done", "event")
	output := add(s, KindFinding, "confirmed", "output")
	edge(parent, event, "derived_from")
	edge(event, output, "yields")
	kinds := []string{"intent", "fact", "finding", "goal", "hint", "digest"}
	boot, err := s.PlannerBootstrap(t.Context(), kinds, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(boot.Records) != 0 {
		t.Fatal("startup preloaded graph", boot)
	}
	var open, running int
	for _, c := range boot.Counts {
		if c.Kind == KindIntent && c.State == "open" {
			open += c.Count
		}
		if c.Kind == KindIntent && c.State == "running" {
			running += c.Count
		}
	}
	if open != 330 || running != 1 {
		t.Fatal(boot.Counts)
	}
	boot, err = s.PlannerBootstrap(t.Context(), kinds, nil, true)
	if err != nil || len(boot.Records) != 1 || boot.Records[0].Ref != fmt.Sprintf("node:%d", old) {
		t.Fatal("older running omitted", boot, err)
	}
	boot, err = s.PlannerBootstrap(t.Context(), kinds, []int64{event, sourceNode, outside, restricted, child, digest}, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(boot)
	for _, secret := range []string{"pending-secret", "ancestor-secret", "digest-secret", "outside-secret", "DO_NOT_PRELOAD"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("leak", secret, string(raw))
		}
	}
	refs := []string{}
	for _, r := range boot.Records {
		refs = append(refs, r.Ref)
	}
	for _, id := range []int64{event, sourceNode, parent, output} {
		if !slices.Contains(refs, fmt.Sprintf("node:%d", id)) {
			t.Fatal("missing related record", id, boot)
		}
	}
	if boot.Records[0].Reason != "event" || boot.Records[len(boot.Records)-1].Reason != "yield" {
		t.Fatal(boot.Records)
	}
	// Source approval alone does not grant inherited read permission.
	sourceAsset, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("source-bootstrap-%d.test", source.ID), TaskID: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	protected := add(d.Exploration(source.ExplorationID), KindFact, "confirmed", "source-protected", sourceAsset)
	if err = d.Assets().RevokeTaskAssets(source.ID, []int64{sourceAsset}, "test", ""); err != nil {
		t.Fatal(err)
	}
	boot, err = s.PlannerBootstrap(t.Context(), kinds, []int64{protected}, false)
	if err != nil || len(boot.Records) != 0 {
		t.Fatal("source revoked leaked", boot, err)
	}
	boot, err = s.PlannerBootstrap(t.Context(), []string{"finding"}, []int64{parent, event}, false)
	if err != nil || len(boot.Records) != 0 {
		t.Fatal("read grants ignored", boot, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.PlannerBootstrap(ctx, kinds, nil, false); err == nil {
		t.Fatal("failure treated as empty")
	}
}

func TestPlannerBootstrapSharedRecordLimit(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	exp, err := d.CreateExploration("records", "goal")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	ids := []int64{}
	for i := 0; i < 20; i++ {
		id, e := s.AddNode(KindFact, map[string]any{"summary": strings.Repeat("中", 10000)}, 0, "confirmed", "test", nil)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	got, err := s.PlannerBootstrap(t.Context(), []string{"fact"}, ids, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 8 || !got.HasMore {
		t.Fatal(got)
	}
	for i, r := range got.Records {
		if r.Ref != fmt.Sprintf("node:%d", ids[len(ids)-1-i]) || len([]rune(r.Summary)) > 240 {
			t.Fatal(r)
		}
	}
}

func TestPlannerBootstrapHeartbeatTimeAndSharedAsset(t *testing.T) {
	d, e := Open(testDSN(t))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	task, e := d.CreateTask("heartbeat", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	asset, e := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("idle-%d.test", task.ID), TaskID: task.ID})
	if e != nil {
		t.Fatal(e)
	}
	ids := []int64{}
	for i := 0; i < 3; i++ {
		id, err := es.AddIntent(map[string]any{"summary": fmt.Sprint(i)}, 1, []int64{asset}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if e = es.SetIntentState(id, "running"); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	// Timestamp order intentionally differs from insertion/ID order.
	for i, days := range []int{1, 30, 10} {
		if _, e = d.Exec(`INSERT INTO activity(exploration_id,node_id,worker,kind,summary,created_at) VALUES($1,$2,'worker','text','activity',now()-($3*interval '1 day'))`, es.ID(), ids[i], days); e != nil {
			t.Fatal(e)
		}
	}
	var before, after string
	if e = d.QueryRow(`SHOW jit`).Scan(&before); e != nil {
		t.Fatal(e)
	}
	out, e := es.PlannerBootstrap(t.Context(), []string{"intent", "fact"}, nil, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.QueryRow(`SHOW jit`).Scan(&after); e != nil || after != before {
		t.Fatal("connection setting leaked", before, after, e)
	}
	if len(out.Records) != 2 {
		t.Fatal(out)
	}
	for _, r := range out.Records {
		if r.Ref == fmt.Sprintf("node:%d", ids[0]) || r.Reason != "idle_running" {
			t.Fatal("wrong idle order", out)
		}
	}
	fact, e := es.AddNode(KindFact, map[string]any{"summary": "asset changed"}, 0, "confirmed", "test", []int64{asset})
	if e != nil {
		t.Fatal(e)
	}
	out, e = es.PlannerBootstrap(t.Context(), []string{"intent", "fact"}, []int64{fact}, false)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Records) != 4 || out.Records[0].Reason != "event" {
		t.Fatal(out)
	}
	for _, r := range out.Records[1:] {
		if r.Reason != "same_asset_active" {
			t.Fatal(out)
		}
	}
}
