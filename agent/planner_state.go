package agent

import (
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
					if !strings.HasPrefix(ref, "planner_state:") {
						continue
					}
					if ri.AgentKey != "planner" || ref != fmt.Sprintf("planner_state:%d", ri.TaskID) || ri.TaskID <= 0 {
						items[i] = map[string]any{"ref": ref, "error": "planner_state unavailable"}
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
	owner, ownerErr := t.store.TaskID()
	if ownerErr != nil || owner != ri.TaskID {
		return actool.Errorf("规划待办任务归属不匹配"), nil
	}
	if err := actool.ValidateInput(t.InputSchema(), in); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	var value struct {
		Todos []actool.Todo `json:"todos"`
	}
	if err := json.Unmarshal(in, &value); err != nil {
		return actool.Errorf("无效待办参数"), nil
	}
	if err := t.store.SavePlannerTodos(ctx, value.Todos); err != nil {
		return actool.Errorf(fmt.Sprintf("待办持久化失败：%v", err)), nil
	}
	res, err := t.CoreTool.Call(ctx, in, tc)
	if err != nil || res.IsError {
		return res, err
	}
	return jsonResult(map[string]any{"message": res.Flatten(), "stored": true, "planner_state_ref": fmt.Sprintf("planner_state:%d", ri.TaskID), "semantics": "replace_whole_list"})
}

func (p *Planner) restoreTodos(ctx context.Context, ts *db.ExplorationStore) (*actool.TodoStore, error) {
	raw, err := ts.PlannerTodos(ctx)
	if err != nil {
		return nil, err
	}
	var items []actool.Todo
	if err = json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	store := p.todoFor(ts.ID())
	input, _ := json.Marshal(map[string]any{"todos": items})
	if _, err = store.Tool().Call(ctx, input, nil); err != nil {
		return nil, err
	}
	return store, nil
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
