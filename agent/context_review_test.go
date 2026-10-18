package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
	"iter"
	"strings"
	"testing"
)

type contextReviewProvider struct {
	captureUsageProvider
	requests *[]llm.CompletionRequest
}

func (p contextReviewProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	*p.requests = append(*p.requests, req)
	return p.captureUsageProvider.Stream(ctx, req)
}

func TestContextHistoryDoesNotInterpretEvidenceAsPlatformIDs(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, e := d.CreateTask("history IDs", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	id, e := s.AddNode(db.KindFact, map[string]any{"summary": "business evidence"}, 0, "confirmed", "test", nil)
	if e != nil {
		t.Fatal(e)
	}
	body := fmt.Sprintf(`{"items":[{"node_id":%d,"kind":"fact","payload":{"entries":[{"field":"/detail","value":{"asset_id":999999999,"node_id":999999998,"evidence":"KEEP BUSINESS JSON","number":9007199254740993}}]}}]}`, id)
	req := llm.CompletionRequest{Messages: []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "read", Name: "context_get", Input: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("read", body, false)}},
	}}
	out, e := FilterTaskAssetRequest(WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: task.ID}), d.Assets(), task.ID, req)
	if e != nil {
		t.Fatal(e)
	}
	result := out.Messages[1].Content[0].Content[0].Text
	if !strings.Contains(result, "KEEP BUSINESS JSON") || !strings.Contains(result, "9007199254740993") {
		t.Fatal("valid evidence was removed/changed:", result)
	}
	if req.Messages[1].Content[0].Content[0].Text != body {
		t.Fatal("audit mutated")
	}
}

func TestAssetReadRequiresTrustedWorkerContext(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, e := d.CreateTaskWithOptions("trusted worker read", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	id, e := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("trusted-%d.test", task.ID), TaskID: task.ID, AgentDiscovered: true})
	if e != nil {
		t.Fatal(e)
	}
	ts := NewToolSet(d.Exploration(task.ExplorationID), "worker")
	ts.SetTaskID(task.ID)
	ts.SetAssetStore(d.Assets(), d.Companies())
	ts.workerExecution = true
	for _, ri := range []RunInfo{{}, {TaskID: task.ID, AgentKey: "planner"}, {TaskID: task.ID + 1, AgentKey: "worker", IntentID: 1}, {TaskID: task.ID, AgentKey: "worker"}, {TaskID: task.ID, AgentKey: "worker", IntentID: 1}} {
		ctx := WithRunInfo(t.Context(), ri)
		r, e := ts.assetGet().Call(ctx, json.RawMessage(fmt.Sprintf(`{"ids":[%d]}`, id)), nil)
		if e != nil || r.IsError {
			t.Fatal(r, e)
		}
		valid := ri.TaskID == task.ID && ri.AgentKey == "worker" && ri.IntentID > 0
		if strings.Contains(r.Flatten(), `"details"`) != valid {
			t.Fatal("asset read role mismatch", ri, r.Flatten())
		}
		r, e = ts.assetSearch().Call(ctx, json.RawMessage(fmt.Sprintf(`{"dsl":"domain==trusted-%d.test"}`, task.ID)), nil)
		if e != nil || r.IsError {
			t.Fatal(r, e)
		}
		if strings.Contains(r.Flatten(), `"target"`) != valid {
			t.Fatal("asset search role mismatch", ri, r.Flatten())
		}
	}
}

func TestWorkerParentContextHasUsableContinuation(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, e := d.CreateTask("parent paging", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	intent, e := s.AddIntent(map[string]any{"summary": "scoped work"}, 1, nil, "test")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 27; i++ {
		id, e := s.AddNode(db.KindFact, map[string]any{"summary": fmt.Sprintf("parent %d", i)}, 0, "confirmed", "test", nil)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.Link(id, db.RelDerivedFrom, intent); e != nil {
			t.Fatal(e)
		}
	}
	ts := NewToolSet(s, "worker")
	ts.SetTaskID(task.ID)
	ts.workerExecution = true
	ctx := WithContextAccess(WithRunInfo(t.Context(), RunInfo{AgentKey: "worker", TaskID: task.ID, IntentID: intent}), ContextAccess{Local: []string{"detail"}})
	text := ts.workerLaunchContext(ctx, intent)
	var page struct {
		Parents []db.ContextHit `json:"parents"`
		Next    json.RawMessage `json:"next"`
	}
	if e = json.Unmarshal([]byte(text[strings.Index(text, "{"):]), &page); e != nil {
		t.Fatal(e)
	}
	if len(page.Parents) != 20 || len(page.Next) == 0 {
		t.Fatal(text)
	}
	seen := map[int64]bool{}
	for _, p := range page.Parents {
		seen[p.ID] = true
	}
	for len(page.Next) > 0 {
		r, e := ts.contextGet().Call(ctx, page.Next, nil)
		if e != nil || r.IsError {
			t.Fatal(r, e)
		}
		var response struct {
			Items []struct {
				Parents []db.ContextHit `json:"parents"`
				Next    json.RawMessage `json:"next"`
				Error   string          `json:"error"`
			} `json:"items"`
		}
		if e = json.Unmarshal([]byte(r.Flatten()), &response); e != nil {
			t.Fatal(e)
		}
		if len(response.Items) != 1 || response.Items[0].Error != "" {
			t.Fatal(r.Flatten())
		}
		for _, p := range response.Items[0].Parents {
			if seen[p.ID] {
				t.Fatal("repeated page", p.ID)
			}
			seen[p.ID] = true
		}
		page.Next = response.Items[0].Next
	}
	if len(seen) != 27 {
		t.Fatal("omitted parent context", len(seen))
	}
}

func TestWorkerPreloadsInheritedAssetWithoutWholeGraph(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, e := d.CreateTask("worker inherited context", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	as := d.Assets()
	host := fmt.Sprintf("inherited-%d.review.test", task.ID)
	a, e := as.UpsertRootDomain(db.UpsertRootDomainReq{TaskID: task.ID, Domain: host})
	if e != nil {
		t.Fatal(e)
	}
	parent, e := es.AddNode(db.KindFact, map[string]any{"summary": "parent evidence"}, 0, "confirmed", "test", []int64{a})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = es.AddNode(db.KindFact, map[string]any{"summary": "UNRELATED_GRAPH_SENTINEL"}, 0, "confirmed", "test", nil); e != nil {
		t.Fatal(e)
	}
	id, e := es.AddIntent(map[string]any{"summary": "verify parent evidence"}, 1, nil, "test")
	if e != nil {
		t.Fatal(e)
	}
	if e = es.Link(parent, db.RelDerivedFrom, id); e != nil {
		t.Fatal(e)
	}
	intent, e := es.GetNode(id)
	if e != nil {
		t.Fatal(e)
	}
	var requests []llm.CompletionRequest
	provider := contextReviewProvider{requests: &requests, captureUsageProvider: captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "done"}, nil) {
			return
		}
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
	}}}
	worker := NewWorker(provider, "fixture", t.TempDir(), nil, 0, 1)
	if _, _, e = worker.Execute(t.Context(), "worker", task.ID, as, es, intent, nil, nil, nil, nil); e != nil {
		t.Fatal(e)
	}
	if len(requests) == 0 {
		t.Fatal("model request missing")
	}
	encoded, _ := json.Marshal(requests[0])
	if !strings.Contains(string(encoded), host) || strings.Contains(string(encoded), "UNRELATED_GRAPH_SENTINEL") {
		t.Fatal("inherited asset missing or whole graph injected", string(encoded))
	}
}

func TestStructuredCapturePreservesExactAuthorizationID(t *testing.T) {
	r, _ := jsonResult(map[string]any{"node_id": int64(9007199254740993), "summary": strings.Repeat("evidence", 2000)})
	out := captureStructuredResult("context_get", r, &actool.ToolContext{MaxOutputChars: 1000})
	var data struct {
		IDs []int64 `json:"node_ids"`
	}
	if e := json.Unmarshal([]byte(out.Flatten()), &data); e != nil {
		t.Fatal(e)
	}
	if len(data.IDs) != 1 || data.IDs[0] != 9007199254740993 {
		t.Fatal("capture changed authorization identity", out.Flatten())
	}
}

func TestPartialContextFailureDoesNotEraseEarlierEvidence(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		req := llm.CompletionRequest{}
		for i, body := range []string{`{"items":[{"ref":"node:1","text":"earlier evidence"}]}`, `{"items":[{"ref":"node:1","error":"query timeout"}]}`} {
			name, in := "context_get", json.RawMessage(`{"items":[{"ref":"node:1"}]}`)
			if deferred {
				name = actool.ExecuteExtraToolName
				in = json.RawMessage(`{"tool_name":"context_get","params":{"items":[{"ref":"node:1"}]}}`)
			}
			id := fmt.Sprint(i)
			req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: name, Input: in}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, body, false)}})
		}
		out := deduplicateToolHistory(req)
		if !strings.Contains(out.Messages[1].Content[0].Content[0].Text, "earlier evidence") {
			t.Fatal("partial failure discarded useful history")
		}
		req.Messages[3].Content[0].Content[0].Text = `{"items":[{"ref":"node:1","text":"new evidence"}]}`
		out = deduplicateToolHistory(req)
		if strings.Contains(out.Messages[1].Content[0].Content[0].Text, "earlier evidence") {
			t.Fatal("successful reread not deduplicated", deferred)
		}
	}
}
