package agent

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/neouks/series/db"
)

func TestPlannerOnDemandContextMetrics(t *testing.T) {
	setup := testDB(t)
	defer setup.Close()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	queries := &contextQueryCounter{}
	cfg.Tracer = queries
	d := &db.DB{DB: stdlib.OpenDB(*cfg)}
	defer d.Close()
	d.SetMaxOpenConns(1)
	for _, n := range []int{10, 1000, 10000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			task, err := d.CreateTask("metrics", "goal", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.DeleteTask(task.ID)
			es := d.Exploration(task.ExplorationID)
			if _, err = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) SELECT $1,CASE n%5 WHEN 0 THEN 'intent' WHEN 1 THEN 'fact' WHEN 2 THEN 'finding' WHEN 3 THEN 'goal' ELSE 'hint' END,CASE n%5 WHEN 0 THEN 'open' WHEN 1 THEN 'confirmed' WHEN 2 THEN 'confirmed' WHEN 3 THEN 'open' ELSE 'active' END,jsonb_build_object('summary','fixture '||n,'detail',repeat('证据正文',2048)) FROM generate_series(1,$2::int)n`, es.ID(), n); err != nil {
				t.Fatal(err)
			}
			var event int64
			if err = d.QueryRow(`SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' ORDER BY id LIMIT 1`, es.ID()).Scan(&event); err != nil {
				t.Fatal(err)
			}
			if _, err = d.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,dst_id,rel) SELECT $1,$2,id,'yields' FROM exploration_nodes WHERE exploration_id=$1 AND kind IN ('fact','finding') ORDER BY id LIMIT 20`, es.ID(), event); err != nil {
				t.Fatal(err)
			}
			ts := NewToolSet(es, "planner")
			ts.SetTaskID(task.ID)
			ts.SetAssetStore(d.Assets(), d.Companies())
			ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: task.ID})
			for _, mode := range []string{"previous_overview", "bootstrap_startup", "bootstrap_event"} {
				elapsed := time.Duration(0)
				alloc := uint64(0)
				sqls, chars, bytes := 0, 0, 0
				for i := 0; i < 3; i++ {
					runtime.GC()
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					queries.n = 0
					start := time.Now()
					var body string
					switch mode {
					case "previous_overview":
						body = renderGraphOverview(ts.graphOverviewData())
					case "bootstrap_startup":
						body = ts.plannerBootstrap(ctx, nil)
					default:
						body = ts.plannerBootstrap(ctx, []TriggerEvent{{Kind: "done", IntentID: event}})
					}
					elapsed += time.Since(start)
					runtime.ReadMemStats(&after)
					alloc += after.TotalAlloc - before.TotalAlloc
					sqls += queries.n
					chars = utf8.RuneCountInString(body)
					bytes = len(body)
					if mode != "previous_overview" && (chars > 8000 || len(body) == 0) {
						t.Fatal("invalid budget")
					}
					if mode != "previous_overview" && strings.Contains(body, `"unavailable"`) {
						t.Fatal("query did not complete", body)
					}
				}
				t.Logf("nodes=%d mode=%s chars=%d bytes=%d sql=%.1f elapsed_avg=%s allocated_avg=%dB", n, mode, chars, bytes, float64(sqls)/3, elapsed/3, alloc/3)
			}
		})
	}
}
