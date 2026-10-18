package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func TestBuiltinSavedDefaultsRespectCurrentBounds(t *testing.T) {
	ts := NewToolSet(nil, "")
	for _, tool := range []actool.CoreTool{ts.contextSearch(), ts.assetSearch(), ts.assetGet(), ts.listTaskAssets()} {
		prop, invalid, valid := "limit", 100, 5
		if tool.Name() == "asset_get" {
			prop, invalid, valid = "max_chars", 99999, 100
		}
		for _, value := range []int{-1, 0, invalid, valid} {
			saved := obj(map[string]any{prop: map[string]any{"default": value}})
			resolved := ResolveBuiltinTool(tool, "", saved)
			defs := scalarDefaults(resolved.InputSchema())
			var got int
			if err := json.Unmarshal(defs[prop], &got); err != nil {
				t.Fatal(err)
			}
			if (value == valid && got != valid) || (value != valid && got == value) {
				t.Fatalf("%s retained invalid default %d: %d", tool.Name(), value, got)
			}
		}
	}
	tool := ResolveBuiltinTool(ts.listTaskAssets(), "", obj(map[string]any{"status": map[string]any{"default": "deleted"}}))
	if string(scalarDefaults(tool.InputSchema())["status"]) != `"approved"` {
		t.Fatal("obsolete enum default accepted")
	}
}

func TestAssetGetTotalBudgetAndContinuation(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("asset budget review", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	var ids []int64
	for i := 0; i < 2; i++ {
		id, e := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("budget-%d-%d.test", task.ID, i), TaskID: task.ID})
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	ts := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	ts.SetTaskID(task.ID)
	ts.SetAssetStore(d.Assets(), d.Companies())
	tool := DecorateTool(ts.assetGet(), "", nil)
	ctx := WithContextAccess(t.Context(), ContextAccess{Local: []string{"assets"}})
	call := func(input any) []map[string]any {
		t.Helper()
		b, _ := json.Marshal(input)
		r, e := tool.Call(ctx, b, &actool.ToolContext{MaxOutputChars: 4000})
		if e != nil || r.IsError || len(r.Flatten()) > 4000 {
			t.Fatal(r.Flatten(), e)
		}
		var out struct {
			Assets []map[string]any `json:"assets"`
		}
		if e = json.Unmarshal([]byte(r.Flatten()), &out); e != nil {
			t.Fatal(e)
		}
		return out.Assets
	}
	rows := call(map[string]any{"ids": ids, "field": "/identity/domain", "max_chars": 1})
	if len(rows) != 2 || rows[1]["error"] == nil {
		t.Fatal("tiny budget must not multiply by batch size", rows)
	}
	if len([]rune(rows[0]["details"].(map[string]any)["value"].(string))) != 1 {
		t.Fatal(rows)
	}
	var joined strings.Builder
	input := map[string]any{"ids": []int64{ids[0], ids[0]}, "field": "/identity/domain", "max_chars": 2}
	for i := 0; ; i++ {
		if i > 100 {
			t.Fatal("non-progressing continuation")
		}
		page := call(input)
		if len(page) != 1 {
			t.Fatal("duplicate asset not deduplicated", page)
		}
		value := page[0]["details"].(map[string]any)["value"].(string)
		if len([]rune(value)) > 2 {
			t.Fatal("continuation lost budget", page)
		}
		joined.WriteString(value)
		next, ok := page[0]["next"].(map[string]any)
		if !ok {
			break
		}
		input = next
	}
	if joined.String() != fmt.Sprintf("budget-%d-0.test", task.ID) {
		t.Fatal("lossy continuation", joined.String())
	}
	for _, ctx := range []struct {
		grants  ContextAccess
		revoked bool
	}{{ContextAccess{}, false}, {ContextAccess{Local: []string{"assets"}}, true}} {
		if ctx.revoked {
			if err := d.Assets().RevokeTaskAssets(task.ID, ids, "test", ""); err != nil {
				t.Fatal(err)
			}
		}
		b, _ := json.Marshal(map[string]any{"ids": ids, "field": "/identity/domain", "max_chars": 100})
		r, e := tool.Call(WithContextAccess(t.Context(), ctx.grants), b, nil)
		if e != nil || strings.Contains(r.Flatten(), fmt.Sprintf("budget-%d", task.ID)) {
			t.Fatal("read bypassed authorization", r.Flatten(), e)
		}
	}
}

func TestOverviewWorkerPolicyMatchesContextReads(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("overview worker policy", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	as, es := d.Assets(), d.Exploration(task.ExplorationID)
	asset, err := as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("overview-review-%d.test", task.ID), TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := es.AddIntent(map[string]any{"summary": "PENDING_VISIBLE_INTENT", "asset_ids": []int64{asset}}, 1, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = es.SetIntentState(intent, "running"); err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(es, "worker")
	ts.SetTaskID(task.ID)
	ts.SetAssetStore(as, d.Companies())
	ts.workerExecution = true
	for _, ri := range []RunInfo{{}, {TaskID: task.ID, AgentKey: "planner"}, {TaskID: task.ID + 1, AgentKey: "worker", IntentID: intent}, {TaskID: task.ID, AgentKey: "worker", IntentID: intent}} {
		r, e := ts.graphOverview().Call(WithRunInfo(t.Context(), ri), json.RawMessage(`{}`), nil)
		if e != nil || r.IsError {
			t.Fatal(r.Flatten(), e)
		}
		want := ri.TaskID == task.ID && ri.AgentKey == "worker" && ri.IntentID == intent
		if strings.Contains(r.Flatten(), "PENDING_VISIBLE_INTENT") != want {
			t.Fatal("overview role policy differs", ri, r.Flatten())
		}
	}
	if err = as.RevokeTaskAssets(task.ID, []int64{asset}, "test", ""); err != nil {
		t.Fatal(err)
	}
	r, e := ts.graphOverview().Call(WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "worker", IntentID: intent}), json.RawMessage(`{}`), nil)
	if e != nil || r.IsError || strings.Contains(r.Flatten(), "PENDING_VISIBLE_INTENT") {
		t.Fatal("cached access survived revocation", r.Flatten(), e)
	}
	states, err := as.TaskAssetApprovalStates(task.ID, []int64{asset})
	if err != nil || states[asset] != db.ApprovalRevoked {
		t.Fatal("read changed approval", states, err)
	}
}
