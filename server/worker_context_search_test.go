package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

// Exercise the reported query through the real Worker assembly, catalog,
// deferred dispatcher and SDK hook. No network or real model calls are needed.
func TestWorkerContextSearchThroughSDK(t *testing.T) {
	isolateStopTestAgentHooks(t)
	s, task := modeServer(t)
	pg := s.m.pg
	_, taskID := stopTestContext(t, task, "worker")
	wireTools(pg, buildDomainReg(s.m.assets))
	previous, found, err := pg.GetSetting(db.ContextCapabilitiesSetting)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if found {
			pg.SetSetting(db.ContextCapabilitiesSetting, previous)
		} else {
			pg.Exec(`DELETE FROM settings WHERE key=$1`, db.ContextCapabilitiesSetting)
		}
	})
	add := func(store *db.ExplorationStore, kind, summary string, assets []int64) int64 {
		t.Helper()
		id, err := store.AddNode(kind, map[string]any{"summary": summary}, 0, "confirmed", "test", assets)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	fact := add(task.Store, db.KindFact, "hsaxh fact", nil)
	finding := add(task.Store, db.KindFinding, "hsaxh finding", nil)
	asset, err := pg.Assets().UpsertIP(db.UpsertIPReq{IP: "192.0.2.173", TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	add(task.Store, db.KindFinding, "hsaxh revoked finding", []int64{asset})
	if err := pg.Assets().RevokeTaskAssets(taskID, []int64{asset}, "test", "fixture"); err != nil {
		t.Fatal(err)
	}
	other, err := s.m.CreateTask("unrelated Worker search fixture", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.m.DeleteTask(other.ID, DeleteTaskOptions{}) })
	add(other.Store, db.KindFinding, "hsaxh other task", nil)
	intentID := stopTestIntent(t, task, "search fixture")
	intent, err := task.Store.GetNode(intentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, allowFact := range []bool{false, true} {
		policy := db.DefaultContextGrants("worker")
		if allowFact {
			policy.Local = append(policy.Local, "fact")
		}
		raw, _ := json.Marshal(map[string]db.ContextToolGrants{"worker": policy})
		if err := pg.SetSetting(db.ContextCapabilitiesSetting, string(raw)); err != nil {
			t.Fatal(err)
		}
		for _, binding := range []bool{false, true} {
			for _, deferred := range []bool{false, true} {
				t.Run(fmt.Sprintf("fact=%v/binding=%v/deferred=%v", allowFact, binding, deferred), func(t *testing.T) {
					agent.FindingTrafficBindingEnabled = func() bool { return binding }
					agent.ToolAugment = nil
					if deferred {
						agent.ToolAugment = func(context.Context, string) ([]actool.CoreTool, agent.DeferredInfo, func()) {
							unlock := actool.NewUnlockSet()
							unlock.Add("context_search")
							return nil, agent.DeferredInfo{Deferred: []string{"context_search"}, Unlock: unlock}, func() {}
						}
					}
					calls := 0
					provider := retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
						calls++
						if calls != 1 {
							return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("查询完成")}}, "end_turn", llm.Usage{}, nil
						}
						if !strings.Contains(strings.Join(req.System, ""), "types仅组合枚举内的值") {
							t.Error("first request lacks permission-aware search guidance")
						}
						queries := []string{
							`{"q":"hsaxh","types":["fact","finding"],"limit":50}`,
							`{"q":"hsaxh","types":["finding"],"limit":50}`,
							`{"q":"hsaxh","limit":50}`,
							fmt.Sprintf(`{"q":"hsaxh","types":["finding"],"task_id":%q,"limit":50}`, other.ID),
						}
						var blocks []llm.ContentBlock
						for i, input := range queries {
							name := "context_search"
							if deferred {
								name = actool.ExecuteExtraToolName
								input = fmt.Sprintf(`{"tool_name":"context_search","params":%s}`, input)
							}
							blocks = append(blocks, llm.ContentBlock{Type: llm.BlockToolUse, ID: fmt.Sprint(i), Name: name, Input: json.RawMessage(input)})
						}
						return llm.Message{Role: llm.RoleAssistant, Content: blocks}, "tool_use", llm.Usage{}, nil
					}}
					worker := agent.NewWorker(provider, "test", t.TempDir(), nil, 100000, 4)
					worker.SetNonStreaming(func() bool { return true })
					results := map[string]db.Activity{}
					_, _, err := worker.Execute(t.Context(), "worker", taskID, pg.Assets(), task.Store, intent, nil, func(a db.Activity) {
						if a.Kind == "tool_result" {
							results[a.ToolUseID] = a
						}
					}, nil, nil)
					if err != nil || calls != 2 || len(results) != 4 {
						t.Fatalf("calls=%d results=%d err=%v", calls, len(results), err)
					}
					for i := range 4 {
						r := results[fmt.Sprint(i)]
						if i == 0 && !allowFact {
							if !r.IsError || !strings.Contains(r.Detail, `Blocked by hook: invalid tool input: input.types[0]: unsupported value; allowed values: ["finding","activity"]`) {
								t.Fatal("missing actionable refusal", r.Detail)
							}
							continue
						}
						if i == 3 {
							if !r.IsError {
								t.Fatal("cross-task query escaped role scope", r.Detail)
							}
							continue
						}
						var output struct {
							Items []db.ContextHit `json:"items"`
						}
						if err := json.Unmarshal([]byte(r.Detail), &output); err != nil || r.IsError {
							t.Fatal(err, r.Detail)
						}
						var ids []int64
						for _, hit := range output.Items {
							ids = append(ids, hit.ID)
						}
						want := []int64{finding}
						if allowFact && i != 1 {
							want = append(want, fact)
						}
						slices.Sort(ids)
						slices.Sort(want)
						if !slices.Equal(ids, want) {
							t.Fatalf("query=%d ids=%v want=%v", i, ids, want)
						}
					}
				})
			}
		}
	}
}
