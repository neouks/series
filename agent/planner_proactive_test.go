package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func TestPlannerProactiveQueriesAreNotAdmissionProof(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("proactive planner", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es, as := d.Exploration(task.ExplorationID), d.Assets()
	ts := NewToolSet(es, "planner")
	ts.SetTaskID(task.ID)
	ts.SetAssetStore(as, d.Companies())
	ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: task.ID})
	call := func(tool actool.CoreTool, in any) actool.Result {
		t.Helper()
		b, _ := json.Marshal(in)
		r, err := tool.Call(ctx, b, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(r.Flatten(), "search_required") {
			t.Fatal("query gate remains", r.Flatten())
		}
		return r
	}
	add, search := ts.addIntent(), ts.contextSearch()
	for _, ri := range []RunInfo{{}, {AgentKey: "planner", TaskID: task.ID + 1}, {AgentKey: "worker", TaskID: task.ID}} {
		r, err := add.Call(WithRunInfo(t.Context(), ri), json.RawMessage(`{"summary":"invalid run must not write"}`), nil)
		if err != nil || !r.IsError || !strings.Contains(r.Flatten(), "运行上下文") {
			t.Fatal("run identity bypass", err, r.Flatten())
		}
	}
	for _, scenario := range []string{"no query", "proactive", "state", "partial", "failed", "other resource"} {
		t.Run(scenario, func(t *testing.T) {
			input := map[string]any{"types": []string{"intent"}, "q": "direction"}
			switch scenario {
			case "state":
				input["state"] = "running"
			case "partial":
				input["limit"] = 1
			case "failed":
				input["cursor"] = "bad"
			case "other resource":
				input["types"] = []string{"fact"}
			}
			if scenario != "no query" {
				r := call(search, input)
				if r.IsError != (scenario == "failed") {
					t.Fatal(r.Flatten())
				}
				if scenario == "partial" && !strings.Contains(r.Flatten(), `"has_more":true`) {
					t.Fatal("fixture needs another page", r.Flatten())
				}
			}
			if r := call(add, map[string]any{"summary": scenario + " direction"}); r.IsError {
				t.Fatal(r.Flatten())
			}
		})
	}
	// No query is needed to reuse an exact live duplicate, including a fresh ToolSet.
	fresh := NewToolSet(es, "planner")
	fresh.SetTaskID(task.ID)
	if r := call(fresh.addIntent(), map[string]any{"summary": "no query direction"}); r.IsError || !strings.Contains(r.Flatten(), "already active") {
		t.Fatal(r.Flatten())
	}
	asset, err := as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("proactive-%d.test", task.ID), TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := es.AddNode(db.KindFact, map[string]any{"summary": "已知父事实"}, 1, "confirmed", "test", []int64{asset})
	if err != nil {
		t.Fatal(err)
	}
	if r := call(add, map[string]any{"summary": "reuse inherited asset", "parent_ids": []int64{parent}}); r.IsError {
		t.Fatal(r.Flatten())
	}
	if err = as.RevokeTaskAssets(task.ID, []int64{asset}, "test", ""); err != nil {
		t.Fatal(err)
	}
	r := call(add, map[string]any{"intents": []any{
		map[string]any{"summary": "revoked inherited", "parent_ids": []int64{parent}},
		map[string]any{"summary": "legal independent"},
	}})
	var batch struct {
		IDs    []int64           `json:"ids"`
		Errors map[string]string `json:"errors"`
	}
	if err = json.Unmarshal([]byte(r.Flatten()), &batch); err != nil {
		t.Fatal(err, r.Flatten())
	}
	if len(batch.IDs) != 2 || batch.IDs[0] != 0 || batch.IDs[1] <= 0 || batch.Errors["0"] == "" {
		t.Fatal(r.Flatten())
	}
	// Denied reads cannot enlarge scope; admission still checks the actual target.
	if r := call(search, map[string]any{"types": []string{"intent"}, "task_id": task.ID + 999}); !r.IsError {
		t.Fatal("cross-task read accepted")
	}
	if _, err = d.Exec(`UPDATE tasks SET execution_mode='manual' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if r := call(add, map[string]any{"summary": "manual denied"}); !r.IsError {
		t.Fatal("manual mode bypassed")
	}
}

func TestPlannerProactivePromptReplacesGateGuidance(t *testing.T) {
	text := plannerDefaultTmpl + plannerRetrievalRule
	for _, stale := range []string{"工具会强制检查", "仅本轮完整查询链有效", "完成该查询后才能新增", "必须先用 context_search"} {
		if strings.Contains(text, stale) {
			t.Fatal(stale)
		}
	}
	for _, guidance := range []string{"准备新增方向时", "仅监督或等待结果", "信息足够后提交", "asset_id", `state=""`, "不要求遍历全图", "查询失败时说明信息缺口"} {
		if !strings.Contains(text, guidance) {
			t.Fatal("missing proactive guidance", guidance)
		}
	}
	prior := PromptOverride
	t.Cleanup(func() { PromptOverride = prior })
	PromptOverride = func(string) (string, bool) { return "custom: search_required must be resolved", true }
	text = plannerSystem("mission", t.TempDir(), t.TempDir()) + plannerRetrievalRule
	if !strings.HasPrefix(text, "custom:") || !strings.Contains(text, "替代旧版查询门槛说明") {
		t.Fatal("custom prompt lost or stale gate uncorrected")
	}
}

func TestGraphOverviewDocumentationIsOnDemand(t *testing.T) {
	tool := NewToolSet(nil, "planner").graphOverview()
	resolved := ResolveBuiltinTool(tool, "Planner每轮已预取，仅需刷新时调用", nil)
	if strings.Contains(resolved.Description(), "每轮已预取") || !strings.Contains(resolved.Description(), "按需") {
		t.Fatal("overview contradicts the lightweight bootstrap", resolved.Description())
	}
}

func TestPlannerWithoutSearchDirectDeferredKeepsPermissions(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("Planner permission review", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	other, err := d.CreateTask("outside task", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(other.ID)
	as := d.Assets()
	asset := func(taskID int64, ip string, approve bool) int64 {
		t.Helper()
		id, err := as.UpsertIP(db.UpsertIPReq{IP: ip, TaskID: taskID, AgentDiscovered: true})
		if err != nil {
			t.Fatal(err)
		}
		if approve {
			if err := as.ApproveTaskAssets(taskID, []int64{id}, "test", ""); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	approved := asset(task.ID, "192.0.2.201", true)
	pending := asset(task.ID, "192.0.2.202", false)
	foreign := asset(other.ID, "192.0.2.203", true)
	ts := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	ts.SetTaskID(task.ID)
	ts.SetAssetStore(as, d.Companies())
	// Revoked/disabled search grants no longer prevent legitimate writes, and
	// they confer no execution rights either. No search is run in this test.
	ctx := WithContextAccess(WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: task.ID}), ContextAccess{})
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%v", deferred), func(t *testing.T) {
			tool := ResolveBuiltinTool(ts.addIntent(), "old query gate", nil)
			invoke := func(ctx context.Context, input any) actool.Result {
				t.Helper()
				raw, _ := json.Marshal(input)
				var result actool.Result
				var err error
				if deferred {
					raw, _ = json.Marshal(map[string]any{"tool_name": "add_intent", "params": json.RawMessage(raw)})
					result, err = actool.NewExecuteExtraTool(actool.NewRegistry(tool), nil).Call(ctx, raw, nil)
				} else {
					result, err = tool.Call(ctx, raw, nil)
				}
				if err != nil || strings.Contains(result.Flatten(), "search_required") {
					t.Fatal(err, result.Flatten())
				}
				return result
			}
			items := []any{}
			for _, id := range []int64{approved, pending, foreign} {
				items = append(items, map[string]any{"summary": fmt.Sprintf("review %v/%d", deferred, id), "asset_ids": []int64{id}})
			}
			r := invoke(ctx, map[string]any{"intents": items})
			var batch struct {
				IDs    []int64           `json:"ids"`
				Errors map[string]string `json:"errors"`
			}
			if err := json.Unmarshal([]byte(r.Flatten()), &batch); err != nil || len(batch.IDs) != 3 || batch.IDs[0] <= 0 || batch.IDs[1] != 0 || batch.IDs[2] != 0 || len(batch.Errors) != 2 {
				t.Fatal(err, r.Flatten())
			}
			// A completed read/previous success is never a lasting authorization.
			if err := as.RevokeTaskAssets(task.ID, []int64{approved}, "test", ""); err != nil {
				t.Fatal(err)
			}
			if r := invoke(ctx, items[0]); !r.IsError {
				t.Fatal("reused approved ID bypassed revocation", r.Flatten())
			}
			if err := as.ApproveTaskAssets(task.ID, []int64{approved}, "test", ""); err != nil {
				t.Fatal(err)
			}
			for _, ri := range []RunInfo{{}, {AgentKey: "planner", TaskID: other.ID}, {AgentKey: "worker", TaskID: task.ID}} {
				// WithRunInfo merges attribution; start without the valid run so
				// the empty case really tests missing identity.
				if r := invoke(WithContextAccess(WithRunInfo(t.Context(), ri), ContextAccess{}), items[0]); !r.IsError {
					t.Fatal("invalid role/context accepted", ri, r.Flatten())
				}
			}
			if _, err := ts.ts.SetExecutionMode(db.ExecutionManual); err != nil {
				t.Fatal(err)
			}
			if r := invoke(ctx, items[0]); !r.IsError {
				t.Fatal("manual mode bypass", r.Flatten())
			}
			if _, err := ts.ts.SetExecutionMode(db.ExecutionManaged); err != nil {
				t.Fatal(err)
			}
		})
	}
	intents, err := ts.ts.ListByKind(db.KindIntent, 100)
	if err != nil || len(intents) != 2 {
		t.Fatal("denied items left residual intents", len(intents), err)
	}
}
