package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

func TestFindingFeedbackDynamicRefresh(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("dynamic feedback", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	ts := NewToolSet(d.Exploration(task.ExplorationID), "worker")
	ts.SetTaskID(task.ID)
	emptyCtx := WithTaskToolSet(WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "worker"}), ts)
	beforeDelete, err := (assetContextProvider{taskID: task.ID}).filter(emptyCtx, llm.CompletionRequest{})
	if err != nil || strings.Contains(strings.Join(beforeDelete.System, "\n"), findingFeedbackPrefix) {
		t.Fatal("unexpected initial feedback", err)
	}
	id, err := d.AddFinding(task.ID, 0, "IDOR", "original", "high", "summary", "private evidence", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := d.DeleteFindingWithFeedback(id, "此接口不再上报")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := d.FindingSuppressionRules(t.Context(), task.ID, 0, 20, false)
	if err != nil || len(rules) != 1 {
		t.Fatal(rules, err)
	}
	page, err := ts.findingDeletionFeedbackPage(0, 20)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(page)
	field, err := ts.listFindingDeletionFeedback().Call(emptyCtx, json.RawMessage(fmt.Sprintf(`{"id":%d}`, feedback.ID)), nil)
	if err != nil || field.IsError {
		t.Fatal(field, err)
	}
	req := llm.CompletionRequest{Messages: []llm.Message{
		{Role: "assistant", Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "feedback-read", Name: "list_finding_deletion_feedback", Input: json.RawMessage(`{}`)}}},
		{Role: "user", Content: []llm.ContentBlock{llm.ToolResultText("feedback-read", string(body), false)}},
		{Role: "assistant", Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "field-read", Name: "list_finding_deletion_feedback", Input: json.RawMessage(fmt.Sprintf(`{"id":%d}`, feedback.ID))}}},
		{Role: "user", Content: []llm.ContentBlock{llm.ToolResultText("field-read", field.Flatten(), false)}},
	}}
	original, _ := json.Marshal(req)
	for _, role := range []string{"worker", "planner", "mainagent"} {
		ctx := WithTaskToolSet(WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: role}), ts)
		p := assetContextProvider{taskID: task.ID}
		out, err := p.filter(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(out.System, "\n"), feedback.Reason) {
			t.Fatal("missing feedback", role)
		}
		out, err = p.filter(ctx, out)
		if err != nil || strings.Count(strings.Join(out.System, "\n"), findingFeedbackPrefix) != 1 {
			t.Fatal("projection accumulated", err)
		}
	}
	if err := d.RevokeFindingSuppression(t.Context(), task.ID, rules[0].ID); err != nil {
		t.Fatal(err)
	}
	ctx := WithTaskToolSet(WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "worker"}), ts)
	out, err := (assetContextProvider{taskID: task.ID}).filter(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(out)
	if strings.Contains(string(data), `\"rule_status\":\"active\"`) || !strings.Contains(string(data), "revoked") {
		t.Fatal("stale rule survived", string(data))
	}
	after, _ := json.Marshal(req)
	if string(after) != string(original) {
		t.Fatal("original request was mutated")
	}
	// A restored/noa-tagged history receives current rule state on the very next
	// request; ref tags and the saved transcript remain intact.
	opts := agentcore.Options{}
	enableNoa(&opts, func() bool { return true }, t.TempDir(), "feedback-worker", nil)
	view := opts.Compactor.(harness.ContextView).View(ctx, req.Messages)
	projected, err := (assetContextProvider{taskID: task.ID}).filter(ctx, llm.CompletionRequest{Messages: view})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(projected)
	if !strings.Contains(string(encoded), "noa-ref") || !strings.Contains(string(encoded), "revoked") || strings.Contains(string(encoded), `\"rule_status\":\"active\"`) {
		t.Fatal("noa lost refresh/ref tags", string(encoded))
	}
	other := WithTaskToolSet(WithRunInfo(t.Context(), RunInfo{TaskID: task.ID + 1, AgentKey: "worker"}), ts)
	isolated, err := (assetContextProvider{taskID: task.ID}).filter(other, req)
	isolatedRaw, _ := json.Marshal(isolated)
	if err != nil || strings.Contains(string(isolatedRaw), feedback.Reason) {
		t.Fatal("cross-task feedback", err)
	}
}

func TestFindingFeedbackRefreshHonorsCancellation(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("feedback cancellation", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	ts := NewToolSet(d.Exploration(task.ExplorationID), "worker")
	ts.SetTaskID(task.ID)
	ctx := WithTaskToolSet(WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "worker"}), ts)
	out, err := (assetContextProvider{taskID: task.ID}).filter(ctx, llm.CompletionRequest{})
	if err != nil || strings.Contains(strings.Join(out.System, "\n"), findingDeletionFeedbackRule) {
		t.Fatal("empty task consumes feedback context", out.System, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (assetContextProvider{taskID: task.ID}).filter(cancelled, llm.CompletionRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal("refresh ignored cancellation", err)
	}
}

func TestFindingFeedbackHistoryBudget(t *testing.T) {
	var req llm.CompletionRequest
	for i := 0; i < 30; i++ {
		id := fmt.Sprint(i)
		body := fmt.Sprintf(`{"feedback_id":%d,"text":%q,"rule_status":"active"}`, i+1, strings.Repeat("界", 800))
		req.Messages = append(req.Messages, llm.Message{Role: "assistant", Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "list_finding_deletion_feedback", Input: json.RawMessage(fmt.Sprintf(`{"id":%d}`, i+1))}}}, llm.Message{Role: "user", Content: []llm.ContentBlock{llm.ToolResultText(id, body, false)}})
	}
	original, _ := json.Marshal(req)
	out := projectFindingFeedbackHistory(req, true)
	bodyChars, omitted := 0, 0
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				for _, c := range b.Content {
					if strings.Contains(c.Text, "history_omitted") {
						omitted++
					} else {
						bodyChars += len([]rune(c.Text))
					}
				}
			}
		}
	}
	if bodyChars > findingDeletionFeedbackBudget || omitted == 0 || len(out.Messages) != len(req.Messages) {
		t.Fatal("history budget/pairs", bodyChars, omitted)
	}
	after, _ := json.Marshal(req)
	if string(after) != string(original) {
		t.Fatal("changed original")
	}
	denied := projectFindingFeedbackHistory(req, false)
	raw, _ := json.Marshal(denied)
	if strings.Contains(string(raw), strings.Repeat("界", 10)) {
		t.Fatal("invalid context retained feedback")
	}
	t.Logf("feedback history: original_bytes=%d projected_body_chars=%d omitted_pages=%d", len(original), bodyChars, omitted)
}
