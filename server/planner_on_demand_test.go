package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestPlannerOnDemandRealRunDirectDeferredAndNoa(t *testing.T) {
	isolateStopTestAgentHooks(t)
	s, task := modeServer(t)
	pg := s.m.pg
	_, taskID := stopTestContext(t, task, "planner")
	wireTools(pg, buildDomainReg(s.m.assets))
	prior, found, err := pg.GetSetting(db.ContextCapabilitiesSetting)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if found {
			pg.SetSetting(db.ContextCapabilitiesSetting, prior)
		} else {
			pg.Exec(`DELETE FROM settings WHERE key=$1`, db.ContextCapabilitiesSetting)
		}
	})
	if err = pg.SetSetting(db.ContextCapabilitiesSetting, `{}`); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"context_search", "add_intent", "graph_overview"} {
		var saved []byte
		if err = pg.QueryRow(`SELECT to_jsonb(t) FROM tools t WHERE key=$1`, key).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			pg.Exec(`DELETE FROM tools WHERE key=$1`, key)
			pg.Exec(`INSERT INTO tools SELECT * FROM jsonb_populate_record(NULL::tools,$1::jsonb)`, string(saved))
		})
		if _, err = pg.Exec(`UPDATE tools SET enabled=true,system=true,agents='["planner"]' WHERE key=$1`, key); err != nil {
			t.Fatal(err)
		}
	}
	for _, deferred := range []bool{false, true} {
		for _, noa := range []bool{false, true} {
			t.Run(fmt.Sprintf("deferred=%v/noa=%v", deferred, noa), func(t *testing.T) {
				agent.ToolAugment = func(context.Context, string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
					info := agent.DeferredInfo{}
					if deferred {
						names := []string{"context_search", "add_intent"}
						info = agent.DeferredInfo{Deferred: names, GlobalNames: names, Unlock: actool.NewUnlockSet(names...)}
					}
					return nil, info, func() {}
				}
				calls, round := 0, 0
				prefix := fmt.Sprintf("direction-%v-%v", deferred, noa)
				var firstInput string
				provider := retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					if calls == 1 {
						firstInput = strings.Join(req.System, "")
						for _, m := range req.Messages {
							for _, b := range m.Content {
								firstInput += b.Text
							}
						}
						if !strings.Contains(firstInput, "准备新增方向时") || !strings.Contains(firstInput, "仅监督或等待结果") || strings.Contains(firstInput, "工具会强制检查") {
							t.Fatal("first input lacks proactive workflow")
						}
					}
					for _, m := range req.Messages {
						for _, b := range m.Content {
							if b.Type == llm.BlockToolResult && b.IsError {
								t.Fatalf("unexpected tool error: %+v", b)
							}
						}
					}
					queryFirst := round != 1 && round != 4
					addTurn := 1
					if queryFirst {
						addTurn = 2
					}
					if round == 4 || calls > addTurn {
						return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("等待已有工作推进")}}, "end_turn", llm.Usage{}, nil
					}
					name := "add_intent"
					params := map[string]any{"summary": fmt.Sprintf("%s round %d", prefix, round)}
					if queryFirst && calls == 1 {
						name = "context_search"
						params = map[string]any{"types": []string{"intent"}, "q": prefix}
						if round == 2 {
							params["state"] = "running"
						}
						if round == 3 {
							params["limit"] = 1
						}
					}
					if deferred {
						params = map[string]any{"tool_name": name, "params": params}
						name = "ExecuteExtraTool"
					}
					raw, _ := json.Marshal(params)
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, Name: name, ID: fmt.Sprintf("call-%d", calls), Input: raw}}}, "tool_use", llm.Usage{}, nil
				}}
				planner := agent.NewPlanner(provider, "test", t.TempDir(), nil, 100000, 8)
				planner.SetNonStreaming(func() bool { return true })
				planner.SetNoaEnabled(func() bool { return noa })
				for round = 0; round < 5; round++ {
					calls = 0
					var commands []string
					_, _, err := planner.Plan(t.Context(), taskID, nil, nil, task.Store, "original-goal", nil, func(a db.Activity) {
						if a.Kind == "command" {
							commands = append(commands, a.Summary)
						}
					})
					if err != nil {
						t.Fatal(err)
					}
					wantCalls := 3
					if round == 1 {
						wantCalls = 2
					}
					if round == 4 {
						wantCalls = 1
					}
					if calls != wantCalls {
						t.Fatal("unexpected requests", calls, commands)
					}
					t.Logf("round=%d mock_model_requests=%d tools=%d failed_retries=0 first_input_chars=%d first_input_bytes=%d", round, calls, calls-1, utf8.RuneCountInString(firstInput), len(firstInput))
				}

				if !strings.Contains(firstInput, "<planner_context>") || strings.Contains(firstInput, "<graph_overview>") || strings.Contains(firstInput, `"reason":"event"`) {
					t.Fatal("startup graph preloaded", firstInput)
				}
			})
		}
	}
}

func TestPlannerNodeEventsCopyIdentities(t *testing.T) {
	task := &Task{notify: make(chan struct{}, 1)}
	ids := []int64{11, 12}
	task.NotifyNodes("goal", ids)
	ids[0] = 999
	task.NotifyFinding(17, "untrusted summary", 18)
	events := task.drainTriggers()
	if len(events) != 2 || events[0].NodeIDs[0] != 11 || events[1].NodeIDs[0] != 18 || events[1].IntentID != 17 {
		t.Fatal(events)
	}
}
