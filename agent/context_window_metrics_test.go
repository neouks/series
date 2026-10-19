package agent

import (
	"encoding/json"
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

func TestContextWindowRetrievalMetrics(t *testing.T) {
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
	for _, nodes := range []int{10, 1000} {
		exp, err := d.CreateExploration("match window metrics", "goal")
		if err != nil {
			t.Fatal(err)
		}
		defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
		if _, err = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) SELECT $1,'fact','confirmed',jsonb_build_object('summary','unrelated','detail',repeat('background',200)) FROM generate_series(1,$2::int)`, exp, nodes); err != nil {
			t.Fatal(err)
		}
		es := d.Exploration(exp)
		id, err := es.AddNode(db.KindFact, map[string]any{"summary": "needle target", "detail": strings.Repeat("中文🙂", 6000) + "UNIQUE_DEEP_MATCH"}, 0, "confirmed", "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		ts := NewToolSet(es, "test")
		for _, direct := range []bool{false, true} {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			queries.n = 0
			start := time.Now()
			calls, bytes := 0, 0
			search, err := ts.contextSearch().Call(t.Context(), json.RawMessage(`{"types":["fact"],"q":"UNIQUE_DEEP_MATCH"}`), nil)
			if err != nil || search.IsError {
				t.Fatal(search.Flatten(), err)
			}
			calls++
			bytes += len(search.Flatten())
			var hits struct{ Items []db.ContextHit }
			json.Unmarshal([]byte(search.Flatten()), &hits)
			if len(hits.Items) != 1 {
				t.Fatal(search.Flatten())
			}
			offset := 0
			if direct {
				offset = max(0, *hits.Items[0].MatchOffset-60)
			}
			for {
				in := json.RawMessage(fmt.Sprintf(`{"items":[{"ref":"node:%d","field":"/detail","offset":%d}]}`, id, offset))
				result, err := ts.contextGet().Call(t.Context(), in, nil)
				if err != nil || result.IsError {
					t.Fatal(result.Flatten(), err)
				}
				calls++
				bytes += len(result.Flatten())
				if strings.Contains(result.Flatten(), "UNIQUE_DEEP_MATCH") {
					break
				}
				var out struct {
					Items []struct {
						Payload struct {
							Next *int `json:"next_offset"`
						}
					}
				}
				json.Unmarshal([]byte(result.Flatten()), &out)
				if len(out.Items) == 0 || out.Items[0].Payload.Next == nil {
					t.Fatal("no continuation")
				}
				offset = *out.Items[0].Payload.Next
			}
			runtime.ReadMemStats(&after)
			t.Logf("nodes=%d hit_window=%t calls=%d sql=%d response_bytes=%d elapsed=%s allocated_bytes=%d", nodes, direct, calls, queries.n, bytes, time.Since(start), after.TotalAlloc-before.TotalAlloc)
			if direct && calls != 2 {
				t.Fatal("hit needs extra reads")
			}
		}
		data := ts.graphOverviewData()
		old := renderLightTaggedOrdered("graph_overview", data, []string{"task", "goals", "open_intents"}, 24000)
		start := time.Now()
		rendered := renderGraphOverview(data)
		t.Logf("nodes=%d overview_before_bytes=%d overview_after_bytes=%d before_chars=%d after_chars=%d new_projection_elapsed=%s", nodes, len(old), len(rendered), utf8.RuneCountInString(old), utf8.RuneCountInString(rendered), time.Since(start))
	}
}
