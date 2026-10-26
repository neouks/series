package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/db"
)

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
