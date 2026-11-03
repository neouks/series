package db

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func graphFixture(t *testing.T) (*DB, *Task, *ExplorationStore) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("incremental graph", "goal", nil, 0, 0)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteTask(task.ID); d.Close() })
	return d, task, d.Exploration(task.ExplorationID)
}

type graphMetricTracer struct{ queries atomic.Int64 }

func (t *graphMetricTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	t.queries.Add(1)
	return ctx
}
func (t *graphMetricTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func repairGraph(t *testing.T, s *ExplorationStore) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	for i := 0; i < 10000; i++ {
		ready, err := s.BootstrapGraphProjection(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ready {
			continue
		}
		n, err := s.MaintainGraphPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("repair did not converge")
}
func TestGraphProjectionRepairRevisionAndDigestValidity(t *testing.T) {
	d, _, s := graphFixture(t)
	root, err := s.AddNode(KindIntent, map[string]any{"summary": "worker"}, 0, "done", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for range 3 {
		id, e := s.AddNode(KindFact, map[string]any{"summary": "observed outcome", "confidence": "observed", "evidence": "private evidence"}, 0, "confirmed", "test", nil)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
		if e = s.Link(root, RelYields, id); e != nil {
			t.Fatal(e)
		}
	}
	before, err := s.BlackboardRevision()
	if err != nil {
		t.Fatal(err)
	}
	repairGraph(t, s)
	if _, err = s.BumpRound(); err != nil {
		t.Fatal(err)
	}
	after, _ := s.BlackboardRevision()
	if before != after {
		t.Fatal("bookkeeping changed semantic revision")
	}
	if _, err = d.Exec(`UPDATE graph_node_projection SET cold_since_at=now()-interval '11 minutes' WHERE exploration_id=$1 AND NOT hot`, s.ID()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.GraphCandidates(t.Context(), 128)
	if err != nil || len(rows) < 3 {
		t.Fatalf("cold candidates=%v err=%v", rows, err)
	}
	versions, err := s.GraphVersions(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := s.PublishGraphDigest(t.Context(), map[string]any{"body": "outcome", "member_ids": map[string]any{"facts": ids}}, ids, versions, before)
	if err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err = d.QueryRow(`SELECT series_digest_sources_current(payload) FROM exploration_nodes WHERE id=$1`, digest).Scan(&valid); err != nil || !valid {
		t.Fatalf("fresh digest valid=%v err=%v", valid, err)
	}
	if rev, _ := s.BlackboardRevision(); rev != before {
		t.Fatal("digest triggered new planner work")
	}
	if _, err = d.Exec(`UPDATE exploration_nodes SET payload=payload||'{"summary":"changed"}'::jsonb WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err = d.QueryRow(`SELECT series_digest_sources_current(payload) FROM exploration_nodes WHERE id=$1`, digest).Scan(&valid); err != nil || valid {
		t.Fatalf("stale digest visible=%v err=%v", valid, err)
	}
	if _, err = s.PublishGraphDigest(t.Context(), map[string]any{"body": "stale"}, ids, versions, before); err == nil {
		t.Fatal("stale generation published")
	}
	for range 2 {
		nav, e := s.ContextNode(t.Context(), digest, "/body", 0, 0, 2000)
		if e != nil || nav["navigation_only"] != true || nav["state"] != "stale" || len(nav["sources"].([]any)) != len(ids) {
			t.Fatal("missing stale navigation", nav, e)
		}
		if _, ok := nav["payload"]; ok {
			t.Fatal("stale summary body returned")
		}
		if e = s.RetireStaleGraphDigests(t.Context()); e != nil {
			t.Fatal(e)
		}
	}
	var evidence string
	if err = d.QueryRow(`SELECT payload->>'evidence' FROM exploration_nodes WHERE id=$1`, ids[0]).Scan(&evidence); err != nil || evidence != "private evidence" {
		t.Fatal("source evidence modified", err)
	}
}

func TestContextLineageAssetIDsRemainPagedAndAuthorized(t *testing.T) {
	d, task, s := graphFixture(t)
	a, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("lineage-%d.test", task.ID), TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.AddNode(KindFact, map[string]any{"summary": "source"}, 0, "confirmed", "test", []int64{a})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := s.AddNode(KindIntent, map[string]any{"summary": "check source"}, 0, "open", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Link(parent, RelDerivedFrom, intent); err != nil {
		t.Fatal(err)
	}
	out, err := s.ContextNode(t.Context(), intent, "/asset_ids", 0, 0, 2000)
	if err != nil {
		t.Fatal(err)
	}
	entries := out["payload"].(map[string]any)["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["value"] != json.Number(fmt.Sprint(a)) {
		t.Fatal("inherited ID not available", out)
	}
	if err = d.Assets().BlockTaskAssets(task.ID, []int64{a}, "user", "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ContextNode(t.Context(), intent, "/asset_ids", 0, 0, 2000); err == nil {
		t.Fatal("blocked lineage disclosed")
	}
}

func TestProjectedPayloadAssetLineageAndLegacyFallback(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	task, err := d.CreateTaskWithOptions("payload lineage", "goal", TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	asset, err := d.Assets().UpsertRootDomain(UpsertRootDomainReq{Domain: fmt.Sprintf("projection-acl-%d.test", task.ID), TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	s := d.Exploration(task.ExplorationID)
	node, err := s.AddNode(KindFact, map[string]any{"summary": "private", "asset_ids": []int64{asset}, "evidence": strings.Repeat("private evidence", 1000)}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ready := range []bool{true, false} {
		if _, err = d.Exec(`UPDATE graph_node_projection SET asset_metadata_ready=$2 WHERE node_id=$1`, node, ready); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ContextNode(t.Context(), node, "/evidence", 0, 0, 1000); err == nil {
			t.Fatal("pending source leaked", ready)
		}
	}
	repairGraph(t, s)
	if err = d.Assets().ApproveTaskAssets(task.ID, []int64{asset}, "user", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ContextNode(t.Context(), node, "/evidence", 0, 0, 1000); err != nil {
		t.Fatal("approved source unavailable", err)
	}
	if err = d.Assets().RevokeTaskAssets(task.ID, []int64{asset}, "user", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ContextNode(t.Context(), node, "/evidence", 0, 0, 1000); err == nil {
		t.Fatal("revoked source leaked")
	}
	if _, err = d.Exec(`UPDATE exploration_nodes SET payload=jsonb_set(payload,'{asset_ids}','["invalid"]') WHERE id=$1`, node); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ContextNode(t.Context(), node, "/evidence", 0, 0, 1000); err == nil {
		t.Fatal("malformed lineage became asset-free")
	}
}

func TestGraphRepairHotRevivalCycleAndPageResume(t *testing.T) {
	d, _, s := graphFixture(t)
	var ids []int64
	for range 270 {
		id, err := s.AddNode(KindFact, map[string]any{"summary": "chain"}, 0, "confirmed", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) > 0 {
			if err = s.Link(ids[len(ids)-1], RelDerivedFrom, id); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, id)
	}
	if err := s.Link(ids[len(ids)-1], RelDerivedFrom, ids[0]); err != nil {
		t.Fatal(err)
	}
	repairGraph(t, s)
	live, err := s.AddNode(KindIntent, map[string]any{"summary": "live"}, 0, "running", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Link(ids[len(ids)-1], RelDerivedFrom, live); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MaintainGraphPage(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A fresh store instance resumes the persisted frontier.
	repairGraph(t, d.Exploration(s.ID()))
	var hot int
	if err = d.QueryRow(`SELECT count(*) FROM graph_node_projection WHERE exploration_id=$1 AND hot`, s.ID()).Scan(&hot); err != nil || hot != 271 {
		t.Fatalf("hot=%d err=%v", hot, err)
	}
	if err = s.SetNodeState(live, "done"); err != nil {
		t.Fatal(err)
	}
	repairGraph(t, s)
	if err = d.QueryRow(`SELECT count(*) FROM graph_node_projection WHERE exploration_id=$1 AND hot`, s.ID()).Scan(&hot); err != nil || hot != 0 {
		t.Fatalf("settled hot=%d err=%v", hot, err)
	}
}

func TestGraphCountCacheAndPlannerStateIsolation(t *testing.T) {
	_, _, s := graphFixture(t)
	rev, _ := s.GraphRevision(t.Context(), true)
	access, e := s.GraphAccessRevision(t.Context())
	if e != nil || graphAccessPart(rev) != access {
		t.Fatal("count snapshot and access epochs differ", rev, access, e)
	}
	if err := s.SaveGraphCounts(t.Context(), "planner:fact", rev, []PlannerCount{{Kind: "fact", State: "confirmed", Count: 7}}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GraphCountCache(t.Context(), "planner:fact", rev)
	if err != nil || !ok || got[0].Count != 7 {
		t.Fatal(got, ok, err)
	}
	s.AddNode(KindFact, map[string]any{"summary": "new"}, 0, "confirmed", "test", nil)
	changed, _ := s.GraphRevision(t.Context(), true)
	if _, ok, err = s.GraphCountCache(t.Context(), "planner:fact", changed); err != nil || ok {
		t.Fatal("stale cache used", err)
	}
	if err = s.SavePlannerTodos(t.Context(), []map[string]any{{"content": "long task todo", "status": "pending"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.PlannerTodos(t.Context())
	if err != nil || !json.Valid(raw) {
		t.Fatal(string(raw), err)
	}
}

func TestGraphRevisionCoalescesBulkTransactionAndRollback(t *testing.T) {
	d, _, s := graphFixture(t)
	before, err := s.BlackboardRevision()
	if err != nil {
		t.Fatal(err)
	}
	var initial int64
	if err = d.QueryRow(`SELECT content_rev FROM graph_revisions WHERE exploration_id=$1`, s.ID()).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) SELECT $1,'fact','confirmed','{"summary":"bulk"}'::jsonb FROM generate_series(1,1000)`, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	var changed int64
	if err = tx.QueryRow(`SELECT content_rev FROM graph_revisions WHERE exploration_id=$1`, s.ID()).Scan(&changed); err != nil || changed != initial+1 {
		t.Fatal("bulk epoch was not coalesced", initial, changed, err)
	}
	if outside, e := s.BlackboardRevision(); e != nil || outside != before {
		t.Fatal("uncommitted revision leaked", outside, e)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if after, e := s.BlackboardRevision(); e != nil || after != before {
		t.Fatal("rollback changed revision", e)
	}
}

func TestLegacyEpochSeedPrecedesLazyMaintenance(t *testing.T) {
	d, _, s := graphFixture(t)
	if _, err := s.AddNode(KindFact, map[string]any{"summary": "legacy record"}, 0, "confirmed", "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM graph_revisions WHERE exploration_id=$1`, s.ID()); err != nil {
		t.Fatal(err)
	}
	// Reopening simulates the startup migration of an existing exploration;
	// metadata backfill is deliberately not part of startup.
	d2, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	var initialized bool
	if err = d2.QueryRow(`SELECT initialized FROM graph_revisions WHERE exploration_id=$1`, s.ID()).Scan(&initialized); err != nil || initialized {
		t.Fatal("legacy epoch not seeded lazily", initialized, err)
	}
	before, err := s.BlackboardRevision()
	if err != nil {
		t.Fatal(err)
	}
	repairGraph(t, s)
	if after, e := s.BlackboardRevision(); e != nil || after != before {
		t.Fatal("maintenance manufactured planner change", before, after, e)
	}
}

func TestConcurrentGraphDigestPublicationAndMetadataEpoch(t *testing.T) {
	d, _, s := graphFixture(t)
	ids := []int64{}
	for range 2 {
		id, err := s.AddNode(KindFact, map[string]any{"summary": "stable source"}, 0, "confirmed", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	repairGraph(t, s)
	versions, err := s.GraphVersions(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := s.BlackboardRevision()
	result := make(chan error, 2)
	for range 2 {
		go func() {
			_, e := s.PublishGraphDigest(t.Context(), map[string]any{"body": "summary", "member_ids": map[string]any{"facts": ids}}, ids, versions, revision)
			result <- e
		}()
	}
	wins := 0
	for range 2 {
		if <-result == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent publication successes=%d", wins)
	}
	digests, err := s.ActiveDigests()
	if err != nil || len(digests) != 1 {
		t.Fatal(digests, err)
	}
	if _, err = d.Exec(`UPDATE graph_revisions SET generation=nextval('graph_projection_generation') WHERE exploration_id=$1`, s.ID()); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err = d.QueryRow(`SELECT series_digest_sources_current(payload) FROM exploration_nodes WHERE id=$1`, digests[0].ID).Scan(&valid); err != nil || valid {
		t.Fatal("rebuilt metadata accepted prior digest", valid, err)
	}
}

func TestGraphPublicationRacingSourceDeletionDoesNotDeadlock(t *testing.T) {
	d, _, s := graphFixture(t)
	for range 10 {
		ids := []int64{}
		for range 2 {
			id, e := s.AddNode(KindFact, map[string]any{"summary": "concurrent source"}, 0, "confirmed", "test", nil)
			if e != nil {
				t.Fatal(e)
			}
			ids = append(ids, id)
		}
		repairGraph(t, s)
		versions, e := s.GraphVersions(t.Context(), ids)
		if e != nil {
			t.Fatal(e)
		}
		rev, _ := s.BlackboardRevision()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		start := make(chan struct{})
		publication := make(chan error, 1)
		deletion := make(chan error, 1)
		go func() {
			<-start
			_, err := s.PublishGraphDigest(ctx, map[string]any{"body": "summary"}, ids, versions, rev)
			publication <- err
		}()
		go func() {
			<-start
			_, err := d.ExecContext(ctx, `DELETE FROM exploration_nodes WHERE id=$1`, ids[0])
			deletion <- err
		}()
		close(start)
		pubErr, delErr := <-publication, <-deletion
		cancel()
		if delErr != nil {
			t.Fatal("source deletion blocked/failed", delErr)
		}
		if pubErr != nil && (strings.Contains(pubErr.Error(), "40P01") || strings.Contains(pubErr.Error(), "deadline")) {
			t.Fatal("publication lock inversion", pubErr)
		}
		var valid int
		if e = d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='digest' AND state='active' AND series_digest_sources_current(payload)`, s.ID()).Scan(&valid); e != nil || valid != 0 {
			t.Fatal("deleted source remained valid", valid, e)
		}
	}
}

func TestGraphPublicationBusySourceDoesNotBlockWriter(t *testing.T) {
	d, _, s := graphFixture(t)
	ids := []int64{}
	for range 2 {
		id, e := s.AddNode(KindFact, map[string]any{"summary": "source"}, 0, "confirmed", "test", nil)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	repairGraph(t, s)
	versions, err := s.GraphVersions(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := s.BlackboardRevision()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT id FROM exploration_nodes WHERE id=$1 FOR UPDATE`, ids[0]); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = s.PublishGraphDigest(ctx, map[string]any{"body": "summary"}, ids, versions, rev); err == nil || !strings.Contains(err.Error(), "55P03") {
		t.Fatal("busy source did not fail without waiting", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM exploration_nodes WHERE id=$1`, ids[0]); err != nil {
		t.Fatal("writer blocked by publication", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestGraphHotFrontierFanoutResumeAndConcurrentWrites(t *testing.T) {
	d, _, s := graphFixture(t)
	live, err := s.AddNode(KindIntent, map[string]any{"summary": "active fanout"}, 0, "running", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.Exec(`WITH members AS (INSERT INTO exploration_nodes(exploration_id,kind,state,payload)
 SELECT $1,'fact','confirmed','{"summary":"fanout"}'::jsonb FROM generate_series(1,2050) RETURNING id)
 INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) SELECT $1,$2,'yields',id FROM members`, s.ID(), live)
	if err != nil {
		t.Fatal(err)
	}
	for {
		ready, e := s.BootstrapGraphProjection(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		if ready {
			break
		}
	}
	for range 3 {
		if _, err = s.MaintainGraphPage(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var queued int
	if err = d.QueryRow(`SELECT count(*) FROM graph_hot_reach WHERE exploration_id=$1`, s.ID()).Scan(&queued); err != nil || queued >= 2051 {
		t.Fatal("fanout was not paged", queued, err)
	}
	// Resume through a fresh store and keep recording isolated facts while the
	// background job repairs its remaining frontier. No body scan enters Go.
	done := make(chan error, 1)
	go func() {
		for range 10 {
			start := time.Now()
			_, e := s.AddNode(KindFact, map[string]any{"summary": "recorded during repair"}, 0, "confirmed", "test", nil)
			if e != nil {
				done <- e
				return
			}
			if time.Since(start) > 5*time.Second {
				done <- fmt.Errorf("recording blocked over 5s")
				return
			}
		}
		done <- nil
	}()
	repairGraph(t, d.Exploration(s.ID()))
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	repairGraph(t, s)
	var hot int
	if err = d.QueryRow(`SELECT count(*) FROM graph_node_projection WHERE exploration_id=$1 AND hot`, s.ID()).Scan(&hot); err != nil || hot != 2051 {
		t.Fatal(hot, err)
	}
	if err = s.SetNodeState(live, "done"); err != nil {
		t.Fatal(err)
	}
	repairGraph(t, s)
	if err = d.QueryRow(`SELECT count(*) FROM graph_node_projection WHERE exploration_id=$1 AND hot`, s.ID()).Scan(&hot); err != nil || hot != 0 {
		t.Fatal(hot, err)
	}
}

func TestGraphLargeProjectionMetrics(t *testing.T) {
	sizes := []int{10, 1000, 10000}
	if os.Getenv("SERIES_GRAPH_STRESS") == "1" {
		sizes = append(sizes, 100000)
	}
	for _, size := range sizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			d, _, s := graphFixture(t)
			cfg, err := pgx.ParseConfig(testDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			tracer := &graphMetricTracer{}
			cfg.Tracer = tracer
			original := d.DB
			d.DB = stdlib.OpenDB(*cfg)
			d.SetMaxOpenConns(1)
			original.Close()
			start := time.Now()
			_, err = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) SELECT $1,
  CASE WHEN x%10=0 THEN 'finding' ELSE 'fact' END,'confirmed',jsonb_build_object('summary','fixture '||x,'evidence',repeat('证据',128)) FROM generate_series(1,$2::int)x`, s.ID(), size)
			if err != nil {
				t.Fatal(err)
			}
			writeElapsed := time.Since(start)
			tracer.queries.Store(0)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start = time.Now()
			repairGraph(t, s)
			repairElapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			repairQueries := tracer.queries.Load()
			tracer.queries.Store(0)
			start = time.Now()
			for range 20 {
				if _, err = s.BlackboardRevision(); err != nil {
					t.Fatal(err)
				}
			}
			revisionElapsed := time.Since(start) / 20
			var pending int
			if err = d.QueryRow(`SELECT count(*) FROM graph_dirty_nodes WHERE exploration_id=$1`, s.ID()).Scan(&pending); err != nil || pending != 0 {
				t.Fatal(pending, err)
			}
			t.Logf("nodes=%d write=%s repair=%s repair_client_sql=%d repair_go_alloc_bytes=%d go_heap_after=%d unchanged_revision_avg=%s revision_client_sql_per_check=%.1f", size, writeElapsed, repairElapsed, repairQueries, after.TotalAlloc-before.TotalAlloc, after.HeapAlloc, revisionElapsed, float64(tracer.queries.Load()-1)/20)
		})
	}
}
