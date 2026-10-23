package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func TestPlannerSearchReceiptScopeAndPagination(t *testing.T) {
	ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: 42})
	makeTool := func() *ToolSet { return &ToolSet{taskID: 42, plannerSearches: &plannerSearchLedger{}} }
	query := contextSearchInput{Types: []string{"intent"}, AssetID: 5}
	for _, name := range []string{"wrong role", "wrong task", "other task", "facts only", "state", "intent", "severity", "prior cursor"} {
		t.Run(name, func(t *testing.T) {
			ts := makeTool()

			q := query
			c := ctx
			switch name {
			case "wrong role":
				c = WithRunInfo(ctx, RunInfo{AgentKey: "worker"})
			case "wrong task":
				c = WithRunInfo(ctx, RunInfo{TaskID: 43})
			case "other task":
				q.TaskID = "43"
			case "facts only":
				q.Types = []string{"fact"}
			case "state":
				q.State = "open"
			case "intent":
				q.IntentID = 9
			case "severity":
				q.Severity = "high"
			case "prior cursor":
				q.Cursor = "prior"
			}
			ts.recordPlannerSearch(c, q, "query", map[string]any{"has_more": false})
			if ts.checkPlannerSearch(ctx, "verify access", []int64{5}) == nil {
				t.Fatal("ineligible search counted")
			}
		})
	}
	ts := makeTool()
	frozen := *ts
	frozen.plannerSearches = ts.plannerSearches.snapshot()
	ts.recordPlannerSearch(ctx, query, "race", map[string]any{"has_more": false})
	if frozen.checkPlannerSearch(ctx, "verify access", []int64{5}) == nil {
		t.Fatal("concurrent search granted an already submitted write")
	}
	ts = makeTool()
	ts.recordPlannerSearch(ctx, query, "query", map[string]any{"has_more": true, "next_cursor": "page2"})
	if ts.checkPlannerSearch(ctx, "verify access", []int64{5}) == nil {
		t.Fatal("partial page accepted")
	}
	q := query
	q.Cursor = "forged-last-page"
	ts.recordPlannerSearch(ctx, q, "query", map[string]any{"has_more": false})
	if ts.checkPlannerSearch(ctx, "verify access", []int64{5}) == nil {
		t.Fatal("skipped page accepted")
	}
	q.Cursor = "page2"
	ts.recordPlannerSearch(ctx, q, "query", map[string]any{"has_more": false})
	if e := ts.checkPlannerSearch(ctx, "verify access", []int64{5}); e != nil {
		t.Fatal(e)
	}
	if ts.checkPlannerSearch(ctx, "verify access", []int64{5, 6}) == nil {
		t.Fatal("unchecked inherited asset accepted")
	}
	if ts.checkPlannerSearch(ctx, "verify access", nil) == nil {
		t.Fatal("asset query covered no-asset direction")
	}
	q = contextSearchInput{Types: []string{"intent"}, Q: "ACCESS"}
	ts.recordPlannerSearch(ctx, q, "keyword", map[string]any{"has_more": false})
	if e := ts.checkPlannerSearch(ctx, "verify access", nil); e != nil {
		t.Fatal(e)
	}
	if ts.checkPlannerSearch(ctx, "different direction", nil) == nil {
		t.Fatal("unrelated keyword accepted")
	}
	if makeTool().checkPlannerSearch(ctx, "verify access", []int64{5}) == nil {
		t.Fatal("cross-round receipt survived")
	}
	// Recording/searching are synchronized, including same-signature parallel reads.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts.recordPlannerSearch(ctx, q, "parallel", map[string]any{"has_more": false})
			_ = ts.checkPlannerSearch(ctx, "verify access", nil)
		}()
	}
	wg.Wait()
}

func TestPlannerSearchGateRealTools(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("planner gate", "original goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	as := d.Assets()
	ts := NewToolSet(es, "planner")
	ts.SetTaskID(task.ID)
	ts.SetAssetStore(as, d.Companies())
	ts.plannerSearches = &plannerSearchLedger{}
	ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: task.ID})
	call := func(tool actool.CoreTool, in any) actool.Result {
		t.Helper()
		b, _ := json.Marshal(in)
		r, e := tool.Call(ctx, b, nil)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	add := ts.addIntent()
	search := ts.contextSearch()
	if r := call(add, map[string]any{"summary": "verify access"}); !r.IsError || !strings.Contains(r.Flatten(), "search_required") {
		t.Fatal(r.Flatten())
	}
	var count int
	d.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent'`, es.ID()).Scan(&count)
	if count != 0 {
		t.Fatal("rejected write left node")
	}
	if r := call(search, map[string]any{"types": []string{"intent"}, "q": "access", "task_id": task.ID}); r.IsError {
		t.Fatal(r.Flatten())
	}
	r := call(add, map[string]any{"intents": []any{map[string]any{"summary": "verify access"}, map[string]any{"summary": "other area"}}})
	var batch struct {
		IDs    []int64           `json:"ids"`
		Errors map[string]string `json:"errors"`
	}
	if e := json.Unmarshal([]byte(r.Flatten()), &batch); e != nil {
		t.Fatal(r.Flatten(), e)
	}
	if len(batch.IDs) != 2 || batch.IDs[0] == 0 || batch.IDs[1] != 0 || batch.Errors["1"] == "" {
		t.Fatal(r.Flatten())
	}
	ts.plannerSearches = &plannerSearchLedger{}
	if r = call(add, map[string]any{"summary": "verify access"}); r.IsError || !strings.Contains(r.Flatten(), "already active") {
		t.Fatal("exact duplicate must reuse", r.Flatten())
	}
	asset, err := as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("gate-%d.test", task.ID), TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := es.AddNode(db.KindFact, map[string]any{"summary": "parent"}, 1, "confirmed", "test", []int64{asset})
	if err != nil {
		t.Fatal(err)
	}
	item := map[string]any{"summary": "inspect inherited direction", "parent_ids": []int64{parent}}
	if r = call(add, item); !r.IsError || !strings.Contains(r.Flatten(), "asset_id") {
		t.Fatal("parent assets not checked", r.Flatten())
	}
	// Failed and state-limited searches never supply proof.
	if r = call(search, map[string]any{"types": []string{"intent"}, "asset_id": asset, "cursor": "bad"}); !r.IsError {
		t.Fatal(r.Flatten())
	}
	if r = call(search, map[string]any{"types": []string{"intent"}, "asset_id": asset, "state": "open"}); r.IsError {
		t.Fatal(r.Flatten())
	}
	if r = call(add, item); !r.IsError {
		t.Fatal("limited search accepted", r.Flatten())
	}
	if r = call(search, map[string]any{"types": []string{"intent", "fact", "activity"}, "asset_id": asset}); r.IsError {
		t.Fatal(r.Flatten())
	}
	if r = call(add, item); r.IsError {
		t.Fatal("mixed successful search not recorded", r.Flatten())
	}
	if err = as.RevokeTaskAssets(task.ID, []int64{asset}, "user", ""); err != nil {
		t.Fatal(err)
	}
	item["summary"] = "new revoked direction"
	if r = call(add, item); !r.IsError {
		t.Fatal("receipt bypassed revocation", r.Flatten())
	}
	// Manual-mode lock remains authoritative after successful search.
	if _, err = d.Exec(`UPDATE tasks SET execution_mode='manual' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	if r = call(search, map[string]any{"types": []string{"intent"}, "q": "manual"}); r.IsError {
		t.Fatal(r.Flatten())
	}
	if r = call(add, map[string]any{"summary": "manual direction"}); !r.IsError {
		t.Fatal("manual mode bypassed", r.Flatten())
	}
}

func TestPlannerBootstrapBudgetAndRequestRevalidation(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, e := d.CreateTask("bootstrap", "original goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	ts := NewToolSet(es, "planner")
	ts.SetTaskID(task.ID)
	asset, e := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("revalidation-%d.test", task.ID), TaskID: task.ID})
	if e != nil {
		t.Fatal(e)
	}
	ids := []int64{}
	for i := 0; i < 12; i++ {
		id, err := es.AddNode(db.KindFact, map[string]any{"summary": strings.Repeat("中🙂", 5000), "detail": "hidden-evidence"}, 0, "confirmed", "test", []int64{asset})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: task.ID})
	text := ts.plannerBootstrap(ctx, []TriggerEvent{{Kind: "finding", NodeIDs: ids, Detail: "untrusted-event-body"}})
	if utf8.RuneCountInString(text) > plannerBootstrapRunes || strings.Contains(text, "hidden-evidence") || strings.Contains(text, "untrusted-event-body") || strings.Count(text, `"reason":"event"`) != 8 {
		t.Fatal(text)
	}
	empty := ts.plannerBootstrap(ctx, nil)
	if strings.Contains(empty, `"reason":`) {
		t.Fatal("no-event preloads", empty)
	}
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock(text)}}}}
	before, _ := json.Marshal(req)
	if e = d.Assets().RevokeTaskAssets(task.ID, []int64{asset}, "user", ""); e != nil {
		t.Fatal(e)
	}
	out, e := (assetContextProvider{assets: d.Assets(), taskID: task.ID}).filter(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out.Messages[0].Content[0].Text, "中🙂") || !strings.Contains(out.Messages[0].Content[0].Text, `"access_changed":true`) {
		t.Fatal(out)
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("original request mutated")
	}
	denied := ContextAccess{Local: []string{"finding"}}
	SetTaskContextReadAccess(WithTaskToolSet(ctx, ts), denied, false)
	text = ts.plannerBootstrap(ctx, []TriggerEvent{{Kind: "finding", NodeIDs: ids}})
	if strings.Contains(text, `"tool":"context_search"`) || strings.Contains(text, `"kind":"fact"`) {
		t.Fatal("disabled search bypassed", text)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	text = ts.plannerBootstrap(cancelled, nil)
	if !strings.Contains(text, `"unavailable"`) {
		t.Fatal("query failure missing", text)
	}
}

func TestPlannerSearchGateActualPaginationAndReplay(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, e := d.CreateTask("pagination", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	for i := 0; i < 3; i++ {
		if _, e = es.AddIntent(map[string]any{"summary": fmt.Sprintf("pagination old %d", i)}, 1, nil, "test"); e != nil {
			t.Fatal(e)
		}
	}
	ts := NewToolSet(es, "planner")
	ts.SetTaskID(task.ID)
	ts.plannerSearches = &plannerSearchLedger{}
	ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: task.ID})
	query := map[string]any{"types": []string{"intent"}, "q": "pagination", "limit": 1}
	var last string
	for i := 0; i < 3; i++ {
		raw, _ := json.Marshal(query)
		r, e := ts.contextSearch().Call(ctx, raw, nil)
		if e != nil || r.IsError {
			t.Fatal(r.Flatten(), e)
		}
		var out map[string]any
		if e = json.Unmarshal([]byte(r.Flatten()), &out); e != nil {
			t.Fatal(e)
		}
		err := ts.checkPlannerSearch(ctx, "pagination new", nil)
		if i < 2 {
			if err == nil || out["has_more"] != true {
				t.Fatal("partial chain counted", out)
			}
			last = out["next_cursor"].(string)
			query["cursor"] = last
		} else if err != nil {
			t.Fatal("completed search not accepted", err)
		}
	}
	ts.plannerSearches = &plannerSearchLedger{}
	query["cursor"] = last
	raw, _ := json.Marshal(query)
	r, e := ts.contextSearch().Call(ctx, raw, nil)
	if e != nil || r.IsError {
		t.Fatal(e, r.Flatten())
	}
	if ts.checkPlannerSearch(ctx, "pagination new", nil) == nil {
		t.Fatal("previous round last-page replay accepted")
	}
	// Output encoding failure must not record a successful query either.
	ts.plannerSearches = &plannerSearchLedger{}
	delete(query, "cursor")
	query["q"] = "no matches anywhere"
	raw, _ = json.Marshal(query)
	r, e = ts.contextSearch().Call(ctx, raw, &actool.ToolContext{MaxOutputChars: 1})
	if e != nil || !r.IsError {
		t.Fatal("output budget error expected", r.Flatten(), e)
	}
	if ts.checkPlannerSearch(ctx, "no matches anywhere", nil) == nil {
		t.Fatal("failed output counted as proof")
	}
}

func TestPlannerCustomPromptKeepsOriginalMission(t *testing.T) {
	prior := PromptOverride
	t.Cleanup(func() { PromptOverride = prior })
	PromptOverride = func(string) (string, bool) { return "custom planner body", true }
	goal := strings.Repeat("完整目标", 3000)
	prompt := plannerSystem(goal, t.TempDir(), t.TempDir())
	if !strings.HasPrefix(prompt, "custom planner body") || !strings.Contains(prompt, goal) {
		t.Fatal("custom prompt lost original goal")
	}
}

func TestPlannerSearchLedgerCapacityAllowsNewQueries(t *testing.T) {
	ts := &ToolSet{taskID: 42, plannerSearches: &plannerSearchLedger{}}
	ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: 42})
	for i := int64(1); i <= 129; i++ {
		ts.recordPlannerSearch(ctx, contextSearchInput{Types: []string{"intent"}, AssetID: i}, fmt.Sprint(i), map[string]any{"has_more": false})
	}
	if len(ts.plannerSearches.queries) != 128 || ts.checkPlannerSearch(ctx, "direction", []int64{129}) != nil || ts.checkPlannerSearch(ctx, "direction", []int64{1}) == nil {
		t.Fatal("capacity did not retain recent queries")
	}
	ts.recordPlannerSearch(ctx, contextSearchInput{Types: []string{"intent"}, AssetID: 1}, "1", map[string]any{"has_more": false})
	if ts.checkPlannerSearch(ctx, "direction", []int64{1}) != nil {
		t.Fatal("reread could not recover evicted proof")
	}
}

func TestPlannerRecoveredPartialBootstrapDoesNotLeak(t *testing.T) {
	ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: 42})
	p := assetContextProvider{taskID: 42}
	for _, body := range []string{`<planner_context>{"summary":"private restored data"}</planner_context>`, `<planner_context>{"records":[{"summary":"private restored data"`, `<planner_context>{invalid private restored data}</planner_context>`} {
		req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock(body)}}}}
		out, e := p.refreshPlannerBootstrap(ctx, req)
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(out.Messages[0].Content[0].Text, "private restored data") || !strings.Contains(out.Messages[0].Content[0].Text, "unavailable") {
			t.Fatal(out)
		}
		if req.Messages[0].Content[0].Text != body {
			t.Fatal("history modified")
		}
	}
}
