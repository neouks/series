package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/neouks/series/db"
	"runtime"
	"testing"
	"time"
)

type contextQueryCounter struct{ n int }

func (c *contextQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n++
	return ctx
}
func (c *contextQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestContextScenarioMetrics(t *testing.T) {
	setup := testDB(t)
	defer setup.Close()
	dsn, _, e := db.DSN()
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	counter := &contextQueryCounter{}
	cfg.Tracer = counter
	d := &db.DB{DB: stdlib.OpenDB(*cfg)}
	defer d.Close()
	d.SetMaxOpenConns(1)
	if e = d.Ping(); e != nil {
		t.Fatal(e)
	}
	for _, count := range []int{10, 1000} {
		exp, e := d.CreateExploration("context performance", "goal")
		if e != nil {
			t.Fatal(e)
		}
		defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
		_, e = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) SELECT $1,'fact','confirmed',jsonb_build_object('summary','fixture '||n,'detail',repeat('证据正文',2048)) FROM generate_series(1,$2::int)n`, exp, count)
		if e != nil {
			t.Fatal(e)
		}
		es := d.Exploration(exp)
		intent, e := es.AddIntent(map[string]any{"summary": "one scoped work"}, 1, nil, "test")
		if e != nil {
			t.Fatal(e)
		}
		_, e = d.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,dst_id,rel) SELECT $1,id,$2,'derived_from' FROM exploration_nodes WHERE exploration_id=$1 AND kind='fact' ORDER BY id LIMIT 2`, exp, intent)
		if e != nil {
			t.Fatal(e)
		}
		ts := NewToolSet(es, "fixture")
		for _, role := range []string{"planner", "worker"} {
			var elapsed time.Duration
			var allocated uint64
			sqls, bytes := 0, 0
			for i := 0; i < 5; i++ {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				counter.n = 0
				start := time.Now()
				var body []byte
				if role == "worker" {
					body = []byte(ts.workerLaunchContext(t.Context(), intent))
				} else {
					body, _ = json.Marshal(ts.graphOverviewData())
				}
				elapsed += time.Since(start)
				runtime.ReadMemStats(&after)
				allocated += after.TotalAlloc - before.TotalAlloc
				sqls += counter.n
				bytes = len(body)
			}
			t.Logf("nodes=%d role=%s initial_context_bytes=%d sql_queries=%.1f elapsed_avg=%s allocated_avg=%dB", count, role, bytes, float64(sqls)/5, elapsed/5, allocated/5)
		}
		// One precise full-text search followed by the selected body field. Report
		// actual round trips, output bytes, memory and elapsed time for this scenario.
		counter.n = 0
		start := time.Now()
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		search, e := ts.contextSearch().Call(t.Context(), json.RawMessage(`{"types":["fact"],"q":"证据正文","limit":10}`), nil)
		if e != nil || search.IsError {
			t.Fatal(search.Flatten(), e)
		}
		var hits struct {
			Items []db.ContextHit `json:"items"`
		}
		if e = json.Unmarshal([]byte(search.Flatten()), &hits); e != nil || len(hits.Items) == 0 {
			t.Fatal(e)
		}
		detail, e := ts.contextGet().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"items":[{"ref":%q,"field":"/detail"}],"max_chars":8000}`, hits.Items[0].Ref)), nil)
		if e != nil || detail.IsError {
			t.Fatal(detail.Flatten(), e)
		}
		responseBytes := len(search.Flatten()) + len(detail.Flatten())

		runtime.ReadMemStats(&m1)
		t.Logf("nodes=%d scenario=body_search_and_read tool_calls=2 sql_queries=%d response_bytes=%d elapsed=%s allocated=%dB", count, counter.n, responseBytes, time.Since(start), m1.TotalAlloc-m0.TotalAlloc)
	}
}
