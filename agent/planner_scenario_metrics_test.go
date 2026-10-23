package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/neouks/series/db"
)

type planningScenarioProvider struct {
	llm.Provider
	t                                    *testing.T
	baseline                             bool
	calls, tools, inputChars, inputBytes int
}

func (p *planningScenarioProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && b.IsError {
				p.t.Fatal("scenario tool failed", b)
			}
		}
	}
	if p.calls == 1 {
		for _, s := range req.System {
			p.inputChars += utf8.RuneCountInString(s)
			p.inputBytes += len(s)
		}
		for _, m := range req.Messages {
			for _, b := range m.Content {
				p.inputChars += utf8.RuneCountInString(b.Text)
				p.inputBytes += len(b.Text)
			}
		}
	}
	if !p.baseline && p.calls == 1 {
		p.tools++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "search", Name: "context_search", Input: json.RawMessage(`{"types":["intent"],"q":"specific-new-direction"}`)}}}, "tool_use", llm.Usage{}, nil
	}
	addTurn := 1
	if !p.baseline {
		addTurn = 2
	}
	if p.calls == addTurn {
		p.tools++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "add", Name: "add_intent", Input: json.RawMessage(`{"summary":"specific-new-direction"}`)}}}, "tool_use", llm.Usage{}, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("已登记方向")}}, "end_turn", llm.Usage{}, nil
}

// The same fixture is also run against an isolated pre-change checkout. The
// test-only flag chooses the previous valid interaction (one add) for that run.
func TestPlannerPlanningScenarioMetrics(t *testing.T) {
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
	q := &contextQueryCounter{}
	cfg.Tracer = q
	d := &db.DB{DB: stdlib.OpenDB(*cfg)}
	defer d.Close()
	d.SetMaxOpenConns(1)
	for _, nodes := range []int{10, 1000} {
		task, err := d.CreateTask("planning metrics", "inspect target", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer d.DeleteTask(task.ID)
		es := d.Exploration(task.ExplorationID)
		if _, err = d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,state,payload) SELECT $1,CASE n%2 WHEN 0 THEN 'intent' ELSE 'fact' END,CASE n%2 WHEN 0 THEN 'done' ELSE 'confirmed' END,jsonb_build_object('summary','old direction '||n,'detail',repeat('context',1000)) FROM generate_series(1,$2::int)n`, es.ID(), nodes); err != nil {
			t.Fatal(err)
		}
		provider := &planningScenarioProvider{t: t, baseline: os.Getenv("SERIES_TEST_PLANNER_BASELINE") == "1"}
		planner := NewPlanner(provider, "mock", t.TempDir(), nil, 100000, 6)
		planner.SetNonStreaming(func() bool { return true })
		q.n = 0
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		if _, _, err = planner.Plan(t.Context(), task.ID, d.Assets(), nil, es, "inspect target", nil, nil); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		runtime.ReadMemStats(&after)
		var count int
		if err = d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND payload->>'summary'='specific-new-direction'`, es.ID()).Scan(&count); err != nil || count != 1 {
			t.Fatal("scenario outcome", count, err)
		}
		t.Logf("nodes=%d baseline=%v text_chars=%d text_bytes=%d sql=%d tools=%d mock_model_requests=%d elapsed=%s allocated=%dB", nodes, provider.baseline, provider.inputChars, provider.inputBytes, q.n-1, provider.tools, provider.calls, elapsed, after.TotalAlloc-before.TotalAlloc)
		want := 2
		if provider.baseline {
			want = 1
		}
		if provider.tools != want {
			t.Fatal(fmt.Sprint(provider.tools))
		}
	}
}
