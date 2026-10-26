package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestRoleRetrievalExamplesExecuteDirectAndDeferred(t *testing.T) {
	s, task := modeServer(t)
	pg := s.m.pg
	_, taskID := stopTestContext(t, task, "planner")
	asset, err := pg.Assets().UpsertIP(db.UpsertIPReq{IP: "192.0.2.1", TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	if err = pg.Assets().ApproveTaskAssets(taskID, []int64{asset}, "test", ""); err != nil {
		t.Fatal(err)
	}
	fact, err := task.Store.AddNode(db.KindFact, map[string]any{"summary": "认证事实"}, 0, "confirmed", "test", []int64{asset})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = task.Store.AddNode(db.KindFinding, map[string]any{"summary": "认证发现"}, 0, "confirmed", "test", []int64{asset}); err != nil {
		t.Fatal(err)
	}
	if _, err = task.Store.AddIntent(map[string]any{"summary": "认证方向"}, 5, []int64{asset}, "test"); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"planner", "worker", "mainagent"} {
		ts := agent.NewToolSet(task.Store, role)
		ts.SetTaskID(taskID)
		ts.SetAssetStore(pg.Assets(), pg.Companies())
		ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: taskID, AgentKey: role})
		catalog := names(ts.AllDomainTools())
		for _, key := range []string{"context_search", "context_get", "asset_search", "asset_get"} {
			t.Run(role+"/"+key, func(t *testing.T) {
				tool := applyContextPolicy(agent.ResolveBuiltinTool(catalog[key], "obsolete instructions", map[string]any{}), defaultContextAccess(role))
				const marker = "调用示例（ID/关键词按当前上下文替换）："
				_, sample, ok := strings.Cut(tool.Description(), marker)
				if !ok {
					t.Fatal("missing permitted example", tool.Description())
				}
				var params map[string]any
				if err := json.Unmarshal([]byte(sample), &params); err != nil {
					t.Fatal(err)
				}
				if key == "asset_get" {
					params["ids"] = []int64{asset}
				}
				if key == "context_get" {
					params["items"] = []any{map[string]any{"ref": fmt.Sprintf("node:%d", fact), "view": "detail"}}
				}
				raw, _ := json.Marshal(params)
				prepared, err := agent.PrepareToolInput(tool, raw)
				if err != nil {
					t.Fatal(err)
				}
				for _, deferred := range []bool{false, true} {
					var result actool.Result
					if deferred {
						extra := actool.NewExecuteExtraTool(actool.NewRegistry(tool), nil)
						input, _ := json.Marshal(map[string]any{"tool_name": key, "params": json.RawMessage(prepared)})
						result, err = extra.Call(ctx, input, nil)
					} else {
						result, err = tool.Call(ctx, prepared, nil)
					}
					if err != nil || result.IsError || strings.Contains(result.Flatten(), `"error"`) {
						t.Fatal(deferred, err, result.Flatten())
					}
					if !strings.Contains(result.Flatten(), "认证") && !strings.Contains(result.Flatten(), "192.0.2.1") {
						t.Fatal("example did not read fixture", result.Flatten())
					}
				}
			})
		}
	}
}

func TestRetrievalExamplesFollowNarrowGrants(t *testing.T) {
	ts := agent.NewToolSet(nil, "test")
	catalog := names(ts.AllDomainTools())
	for _, spec := range []struct {
		name     string
		grants   []string
		fragment string
	}{
		{"context_search", []string{"finding"}, `"types":["finding"]`},
		{"context_search", []string{"activity"}, `"intent_id":123`},
		{"context_get", []string{"output"}, `"view":"output"`},
		{"context_get", []string{"activity_detail"}, `"ref":"activity:123"`},
		{"asset_search", []string{"untested"}, `{"untested":true}`},
	} {
		tool := applyContextPolicy(catalog[spec.name], agent.ContextAccess{Local: spec.grants})
		if !strings.Contains(tool.Description(), spec.fragment) {
			t.Fatal(tool.Description())
		}
		_, raw, ok := strings.Cut(tool.Description(), "调用示例（ID/关键词按当前上下文替换）：")
		if !ok {
			t.Fatal("missing example")
		}
		if _, err := agent.PrepareToolInput(tool, json.RawMessage(raw)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"context_search", "context_get", "asset_search", "asset_get"} {
		tool := applyContextPolicy(catalog[name], agent.ContextAccess{})
		if strings.Contains(tool.Description(), "调用示例") {
			t.Fatal("unauthorized example", name)
		}
	}
}
