package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestPlannerIntentSearchThroughPlan(t *testing.T) {
	isolateStopTestAgentHooks(t)
	s, task := modeServer(t)
	pg := s.m.pg
	_, taskID := stopTestContext(t, task, "planner")
	wireTools(pg, buildDomainReg(s.m.assets))
	var original []byte
	if err := pg.QueryRow(`SELECT to_jsonb(t) FROM tools t WHERE key='context_search'`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pg.Exec(`DELETE FROM tools WHERE key='context_search'`)
		pg.Exec(`INSERT INTO tools SELECT * FROM jsonb_populate_record(NULL::tools,$1::jsonb)`, string(original))
	})
	for _, key := range []string{db.ContextCapabilitiesSetting, "tool_planner_intent_search_v1"} {
		value, found, err := pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if found {
				pg.SetSetting(key, value)
			} else {
				pg.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		})
	}
	if _, err := pg.Exec(`UPDATE tools SET system=true,enabled=true,agents='["planner"]' WHERE key='context_search'`); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetSetting(db.ContextCapabilitiesSetting, `{"planner":{"local":["fact","finding","goal","activity","detail"],"cross":[]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`DELETE FROM settings WHERE key='tool_planner_intent_search_v1'`); err != nil {
		t.Fatal(err)
	}
	intent := func(task *Task, state string) int64 {
		t.Helper()
		id := stopTestIntent(t, task, state+" search fixture")
		if err := task.Store.SetIntentState(id, state); err != nil {
			t.Fatal(err)
		}
		return id
	}
	running := intent(task, "running")
	queued := intent(task, "open")
	intent(task, "done")
	other, err := s.m.CreateTask("unrelated intent search fixture", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.DeleteTask(other.ID, DeleteTaskOptions{}) })
	intent(other, "running")

	// Exercise the real Planner assembly and SDK input hook, with a local fake
	// provider. The finding workflow wrapper previously rejected this exact input.
	call := func(t *testing.T, raw string) db.Activity {
		t.Helper()
		calls := 0
		provider := retestProvider{complete: func(_ context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			calls++
			if calls == 1 {
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "intent-search", Name: "context_search", Input: json.RawMessage(raw)}}}, "tool_use", llm.Usage{}, nil
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("查询完成")}}, "end_turn", llm.Usage{}, nil
		}}
		planner := agent.NewPlanner(provider, "test", t.TempDir(), nil, 100000, 4)
		planner.SetNonStreaming(func() bool { return true })
		var results []db.Activity
		_, _, err := planner.Plan(t.Context(), taskID, nil, nil, task.Store, "goal", nil, func(a db.Activity) {
			if a.Kind == "tool_result" && a.ToolUseID == "intent-search" {
				results = append(results, a)
			}
		})
		if err != nil || len(results) != 1 {
			t.Fatalf("Planner result count=%d error=%v", len(results), err)
		}
		return results[0]
	}
	const input = `{"types": ["intent"], "state": "running", "limit": 20}`
	agent.FindingTrafficBindingEnabled = func() bool { return true }
	if r := call(t, input); !r.IsError || !strings.Contains(r.Detail, "Blocked by hook: invalid tool input: input.types[0]: unsupported value") {
		t.Fatal("legacy policy did not reproduce the reported hook failure", r.Detail)
	}
	if err := pg.MigratePlannerIntentSearch(); err != nil {
		t.Fatal(err)
	}
	assertIDs := func(t *testing.T, raw string, want ...int64) {
		t.Helper()
		r := call(t, raw)
		if r.IsError {
			t.Fatal(r.Detail)
		}
		var result struct {
			Items []db.ContextHit `json:"items"`
		}
		if err := json.Unmarshal([]byte(r.Detail), &result); err != nil {
			t.Fatal(err, r.Detail)
		}
		var ids []int64
		for _, hit := range result.Items {
			if hit.Kind != "intent" || hit.TaskID != taskID || hit.Ref != fmt.Sprintf("node:%d", hit.ID) {
				t.Fatal("unexpected result scope", hit)
			}
			ids = append(ids, hit.ID)
		}
		slices.Sort(ids)
		slices.Sort(want)
		if !slices.Equal(ids, want) {
			t.Fatalf("got %v want %v: %s", ids, want, r.Detail)
		}
	}
	for _, trafficBinding := range []bool{true, false} {
		t.Run(fmt.Sprintf("traffic_binding=%v", trafficBinding), func(t *testing.T) {
			agent.FindingTrafficBindingEnabled = func() bool { return trafficBinding }
			assertIDs(t, input, running)
			assertIDs(t, `{"types":["intent"],"state":"open","limit":20}`, queued)
			for _, rejected := range []string{
				fmt.Sprintf(`{"types":["intent"],"state":"running","task_id":%s}`, other.ID),
				`{"types":["hint"]}`,
			} {
				if r := call(t, rejected); !r.IsError {
					t.Fatal("migration enlarged unrelated permissions", r.Detail)
				}
			}
		})
	}
	// Fresh installations use defaults without a persisted capability map.
	if _, err := pg.Exec(`DELETE FROM settings WHERE key=$1`, db.ContextCapabilitiesSetting); err != nil {
		t.Fatal(err)
	}
	assertIDs(t, input, running)
}
