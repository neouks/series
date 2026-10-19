package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
)

const findingFeedbackPrefix = "当前任务漏洞删除反馈（动态刷新）："

func feedbackRoleAllowed(role string) bool {
	return role == "planner" || role == "worker" || role == "mainagent"
}

func (p assetContextProvider) refreshFindingFeedback(ctx context.Context, req llm.CompletionRequest) (llm.CompletionRequest, error) {
	// Drop any prior projection, even if the caller changed task/role. This never
	// edits the SDK transcript or original messages.
	system := make([]string, 0, len(req.System))
	for _, s := range req.System {
		if s != findingDeletionFeedbackRule && !strings.HasPrefix(s, findingFeedbackPrefix) {
			system = append(system, s)
		}
	}
	req.System = system
	ri := RunInfoFrom(ctx)
	if !feedbackRoleAllowed(ri.AgentKey) || ri.TaskID != p.taskID || p.taskID <= 0 {
		return projectFindingFeedbackHistory(req, false), nil
	}
	ts, _ := ctx.Value(taskToolSetContextKey{}).(*ToolSet)
	if ts == nil || ts.ts == nil || ts.taskID != p.taskID {
		return projectFindingFeedbackHistory(req, false), nil
	}
	req = projectFindingFeedbackHistory(req, true)
	page, err := ts.findingDeletionFeedbackPageContext(ctx, 0, 20)
	if err != nil {
		return req, fmt.Errorf("刷新漏洞删除反馈失败: %w", err)
	}
	if len(page.Groups) > 0 {
		req.System = append(req.System, findingDeletionFeedbackRule)
		raw, _ := json.Marshal(page)
		req.System = append(req.System, findingFeedbackPrefix+string(raw))
	}
	// Refresh state only: historical reason/title fields are immutable, while
	// revocations must not leave stale active flags in a prior tool result.
	type hit struct {
		m, b, c int
		page    deletionFeedbackPage
		field   map[string]any
		id      int64
	}
	var hits []hit
	var ids []int64
	seen := map[int64]bool{}
	calls := map[string]bool{}
	for mi, m := range req.Messages {
		for bi, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				calls[b.ID] = assetContextCall(b).Name == "list_finding_deletion_feedback"
			}
			if b.Type != llm.BlockToolResult || b.IsError || !calls[b.ToolUseID] {
				continue
			}
			for ci, c := range b.Content {
				if c.Type != llm.BlockText {
					continue
				}
				var page deletionFeedbackPage
				if json.Unmarshal([]byte(c.Text), &page) != nil {
					continue
				}
				if page.Groups == nil {
					var field map[string]any
					var identity struct {
						ID int64 `json:"feedback_id"`
					}
					if json.Unmarshal([]byte(c.Text), &identity) == nil && json.Unmarshal([]byte(c.Text), &field) == nil {
						if id := identity.ID; id > 0 {
							field["feedback_id"] = id
							hits = append(hits, hit{mi, bi, ci, page, field, id})
							if !seen[id] {
								ids = append(ids, id)
								seen[id] = true
							}
						}
					}
					continue
				}
				hits = append(hits, hit{mi, bi, ci, page, nil, 0})
				for _, g := range page.Groups {
					for _, f := range g.Findings {
						if !seen[f.ID] {
							ids = append(ids, f.ID)
							seen[f.ID] = true
						}
					}
				}
			}
		}
	}
	if len(ids) == 0 {
		return req, nil
	}
	states, err := ts.ts.FindingFeedbackStates(ctx, ids)
	if err != nil {
		return req, err
	}
	req = cloneToolMessages(req)
	for _, h := range hits {
		if h.field != nil {
			id := h.id
			if state, ok := states[id]; ok {
				h.field["rule_id"], h.field["rule_status"] = state.RuleID, state.RuleStatus
			} else {
				h.field = map[string]any{"feedback_id": id, "rule_status": "unavailable"}
			}
			b := &req.Messages[h.m].Content[h.b]
			b.Content = append([]llm.ContentBlock(nil), b.Content...)
			raw, _ := json.Marshal(h.field)
			b.Content[h.c].Text = string(raw)
			continue
		}
		for gi := range h.page.Groups {
			visible := false
			for fi := range h.page.Groups[gi].Findings {
				f := &h.page.Groups[gi].Findings[fi]
				if current, ok := states[f.ID]; ok {
					visible = true
					f.RuleID, f.RuleStatus = current.RuleID, current.RuleStatus
				} else {
					*f = deletionFeedbackIdentity{ID: f.ID, RuleStatus: "unavailable"}
				}
			}
			if !visible {
				h.page.Groups[gi].Reason = ""
			}
		}
		b := &req.Messages[h.m].Content[h.b]
		b.Content = append([]llm.ContentBlock(nil), b.Content...)
		raw, _ := json.Marshal(h.page)
		b.Content[h.c].Text = string(raw)
	}
	return req, nil
}

// Feedback pagination must not accumulate an unbounded second rules prompt.
// Keep the newest successful read bodies and retain older calls for rereading.
// A missing/mismatched trusted context must also hide historical feedback.
func projectFindingFeedbackHistory(req llm.CompletionRequest, allowed bool) llm.CompletionRequest {
	type position struct{ m, b int }
	calls := map[string]bool{}
	var results []position
	for mi, m := range req.Messages {
		for bi, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				calls[b.ID] = assetContextCall(b).Name == "list_finding_deletion_feedback"
			}
			if b.Type == llm.BlockToolResult && calls[b.ToolUseID] && !b.IsError {
				results = append(results, position{mi, bi})
			}
		}
	}
	if len(results) == 0 {
		return req
	}
	req = cloneToolMessages(req)
	remaining := findingDeletionFeedbackBudget
	for i := len(results) - 1; i >= 0; i-- {
		p := results[i]
		b := &req.Messages[p.m].Content[p.b]
		size := 0
		for _, c := range b.Content {
			if c.Type == llm.BlockText {
				size += utf8.RuneCountInString(c.Text)
			}
		}
		if allowed && size <= remaining {
			remaining -= size
			continue
		}
		placeholder := `{"history_omitted":true,"read_from_original_call":true,"rule_status":"refresh_required"}`
		if !allowed {
			placeholder = `{"feedback_unavailable":true}`
		}
		b.Content = []llm.ContentBlock{{Type: llm.BlockText, Text: placeholder}}
		remaining = 0
	}
	return req
}
