package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

type persistentPlannerTodo struct {
	actool.CoreTool
	store *db.ExplorationStore
}

func (t persistentPlannerTodo) Description() string {
	return t.CoreTool.Description() + " 完整替换持久化规划清单；当前预览不含全部待办，需要时先用context_get读取planner_state:当前任务ID的/todos，不能用预览覆盖未读事项。"
}

func refreshPlannerStateHistory(ctx context.Context, req llm.CompletionRequest) llm.CompletionRequest {
	ri := RunInfoFrom(ctx)
	calls := map[string]bool{}
	cloned := false
	for mi, m := range req.Messages {
		for bi, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				calls[b.ID] = assetContextCall(b).Name == "context_get"
			}
			if b.Type != llm.BlockToolResult || !calls[b.ToolUseID] {
				continue
			}
			for ci, c := range b.Content {
				if c.Type != llm.BlockText {
					continue
				}
				var result map[string]any
				if decodeStructuredJSON([]byte(c.Text), &result) != nil {
					continue
				}
				items, ok := result["items"].([]any)
				if !ok {
					continue
				}
				changed := false
				for i, v := range items {
					row, ok := v.(map[string]any)
					if !ok {
						continue
					}
					ref, _ := row["ref"].(string)
					prefix := "planner_state:"
					if strings.HasPrefix(ref, "planner_changes:") {
						prefix = "planner_changes:"
					}
					if !strings.HasPrefix(ref, prefix) {
						continue
					}
					if ri.AgentKey != "planner" || ref != fmt.Sprintf("%s%d", prefix, ri.TaskID) || ri.TaskID <= 0 {
						items[i] = map[string]any{"ref": ref, "error": strings.TrimSuffix(prefix, ":") + " unavailable"}
						changed = true
					}
				}
				if changed {
					if !cloned {
						req = cloneToolMessages(req)
						cloned = true
					}
					raw, _ := json.Marshal(result)
					block := &req.Messages[mi].Content[bi]
					block.Content = append([]llm.ContentBlock(nil), block.Content...)
					block.Content[ci].Text = string(raw)
				}
			}
		}
	}
	return req
}

func (t persistentPlannerTodo) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	ri := RunInfoFrom(ctx)
	if ri.AgentKey != "planner" || ri.ExplorationID != t.store.ID() || ri.TaskID <= 0 {
		return actool.Errorf("规划待办需要匹配的Planner运行上下文"), nil
	}
	owner, ownerErr := t.store.TaskIDContext(ctx)
	if ownerErr != nil || owner != ri.TaskID {
		return actool.Errorf("规划待办任务归属不匹配"), nil
	}
	if err := actool.ValidateInput(t.InputSchema(), in); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	var value struct {
		Todos json.RawMessage `json:"todos"`
	}
	if err := json.Unmarshal(in, &value); err != nil {
		return actool.Errorf("无效待办参数"), nil
	}
	if err := validatePlannerTodos(ctx, value.Todos); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	// Reuse official validation/acknowledgement without retaining a full list
	// in the tool closure for the rest of this planning round.
	res, err := actool.NewTodoStore().Tool().Call(ctx, in, tc)
	if err != nil || res.IsError {
		return res, err
	}
	if err := t.store.SavePlannerTodos(ctx, value.Todos); err != nil {
		return actool.Errorf(fmt.Sprintf("待办持久化失败：%v", err)), nil
	}
	return jsonResult(map[string]any{"message": res.Flatten(), "stored": true, "planner_state_ref": fmt.Sprintf("planner_state:%d", ri.TaskID), "semantics": "replace_whole_list"})
}

// The official basic schema validator does not recurse into array item enums.
// Validate one item at a time without retaining another complete typed list.
func validatePlannerTodos(ctx context.Context, raw json.RawMessage) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('[') {
		return fmt.Errorf("todos必须为数组；清空请提供[]")
	}
	for index := 0; d.More(); index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var item struct {
			Content *string
			Status  *actool.TodoStatus
		}
		if err := d.Decode(&item); err != nil || item.Content == nil || item.Status == nil {
			return fmt.Errorf("todos[%d]需要字符串content和status", index)
		}
		switch *item.Status {
		case actool.TodoPending, actool.TodoInProgress, actool.TodoCompleted:
		default:
			return fmt.Errorf("todos[%d].status仅支持pending/in_progress/completed", index)
		}
	}
	_, err = d.Token()
	return err
}

type plannerTodoCached struct {
	stamp, text string
	used        uint64
}

func (p *Planner) plannerTodos(ctx context.Context, ts *db.ExplorationStore) (string, error) {
	p.todoMu.Lock()
	previous := p.todoPreviews[ts.ID()]
	p.todoMu.Unlock()
	state, err := ts.PlannerTodoSnapshot(ctx, previous.stamp)
	if err != nil {
		return "", err // Never use a cached preview on a failed live read.
	}
	text := previous.text
	if !state.Unchanged {
		rows := make([]map[string]any, 0, len(state.Items))
		for _, item := range state.Items {
			rows = append(rows, map[string]any{"index": item.Index, "content": item.Content, "status": item.Status, "truncated": item.TotalChars > utf8.RuneCountInString(item.Content)})
		}
		text = formatPlannerTodoPreview(state.Counts, rows, state.Total)
	}
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	if p.todoPreviews == nil {
		p.todoPreviews = map[int64]plannerTodoCached{}
	}
	if _, exists := p.todoPreviews[ts.ID()]; !exists && len(p.todoPreviews) >= 16 {
		var oldestID int64
		oldest := ^uint64(0)
		for id, item := range p.todoPreviews {
			if item.used < oldest {
				oldest, oldestID = item.used, id
			}
		}
		delete(p.todoPreviews, oldestID)
	}
	p.todoPreviewClock++
	p.todoPreviews[ts.ID()] = plannerTodoCached{stamp: state.Stamp, text: text, used: p.todoPreviewClock}
	return text, nil
}

func formatPlannerTodoPreview(counts any, candidates []map[string]any, total int64) string {
	selected := []map[string]any{}
	for _, row := range candidates {
		trial := append(append([]map[string]any{}, selected...), row)
		raw, _ := json.Marshal(trial)
		if utf8.RuneCount(raw) <= 2400 && len(trial) <= 12 {
			selected = trial
		}
	}
	raw, _ := json.Marshal(map[string]any{"counts": counts, "items": selected, "is_snapshot": true, "has_more": int64(len(selected)) < total, "full_replace_requires_complete_todos": true})
	return "\n\n【规划待办预览】" + string(raw) + "\n未展示项仍保留；TodoWrite是完整替换，不能用截断预览覆盖完整清单。需要时按下面planner_state引用，携带index分页读取。"
}

func plannerTodoPreview(items []actool.Todo) string {
	counts := map[actool.TodoStatus]int{}
	for _, it := range items {
		counts[it.Status]++
	}
	selected := []map[string]any{}
	for _, status := range []actool.TodoStatus{actool.TodoInProgress, actool.TodoPending} {
		for index, it := range items {
			if it.Status != status {
				continue
			}
			runes := []rune(it.Content)
			preview := string(runes[:min(500, len(runes))])
			row := map[string]any{"index": index, "content": preview, "status": it.Status, "truncated": len(runes) > 500}
			trial := append(append([]map[string]any{}, selected...), row)
			raw, _ := json.Marshal(trial)
			if utf8.RuneCount(raw) > 2400 || len(trial) > 12 {
				continue
			}
			selected = trial
		}
	}
	raw, _ := json.Marshal(map[string]any{"counts": counts, "items": selected, "is_snapshot": true, "has_more": len(selected) < len(items), "full_replace_requires_complete_todos": true})
	return "\n\n【规划待办预览】" + string(raw) + "\n未展示项仍保留；TodoWrite是完整替换，不能用截断预览覆盖完整清单。需要时按下面planner_state引用，携带index分页读取。"
}

func (t *ToolSet) readPlannerState(ctx context.Context, task string, item contextRead, budget int) (map[string]any, error) {
	ri := RunInfoFrom(ctx)
	id, err := strconv.ParseInt(strings.TrimPrefix(item.Ref, "planner_state:"), 10, 64)
	if err != nil || id <= 0 || id != t.taskID || ri.TaskID != t.taskID || ri.AgentKey != "planner" || t.ts == nil {
		return nil, fmt.Errorf("planner_state仅允许Planner读取当前任务")
	}
	owner, ownerErr := t.ts.TaskID()
	if ownerErr != nil {
		return nil, ownerErr
	}
	if owner != t.taskID {
		return nil, fmt.Errorf("planner_state任务归属不匹配")
	}
	if task != "" && task != fmt.Sprint(id) {
		return nil, fmt.Errorf("planner_state不支持跨任务读取")
	}
	if item.View != "" && item.View != "detail" || item.Before != 0 || item.Offset < 0 || item.Index < 0 || item.Field != "" && item.Field != "/todos" {
		return nil, fmt.Errorf("planner_state仅支持detail、/todos和index/offset")
	}
	if _, err = t.contextTarget(ctx, "", "detail"); err != nil {
		return nil, err
	}
	items, total, err := t.ts.PlannerTodoPage(ctx, item.Index, item.Offset)
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	used := 0
	nextIndex := item.Index
	offset := item.Offset
	nextOffset := 0
	for _, preview := range items {
		i := preview.Index
		runes := []rune(preview.Content)
		if offset > preview.TotalChars {
			return nil, fmt.Errorf("待办文本位置越界")
		}
		n := min(len(runes), max(0, budget-used-180))
		if budget-used < 180 {
			break
		}
		rows = append(rows, map[string]any{"index": i, "status": preview.Status, "content": string(runes[:n]), "offset": offset, "truncated": offset+n < preview.TotalChars})
		used += n + 180
		nextIndex = i + 1
		if offset+n < preview.TotalChars {
			nextIndex = i
			nextOffset = offset + n
			break
		}
		offset = 0
	}
	out := map[string]any{"ref": item.Ref, "items": rows, "has_more": nextIndex < total}
	if nextIndex < total {
		out["next"] = map[string]any{"items": []contextRead{{Ref: item.Ref, Field: "/todos", Index: nextIndex, Offset: nextOffset}}}
	}
	return out, nil
}
