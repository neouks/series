package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/db"
)

const plannerBootstrapRunes = 8000
const plannerRetrievalRule = `
【Planner 按需读取规则，优先于旧版概览说明】本轮只提供任务基本信息、授权可见计数、本轮事件和少量相关摘要，不是完整探索图。先读与当前目标/事件相关的 goal、hint、fact、finding、digest；正文用 context_get，不能根据计数或未展示就判定已达成/不存在。不要重复拉全图或遍历全历史。
新增意图前必须实际调用 context_search，types 包含 intent，不设置 state/intent_id/severity。按候选的每个显式及父节点继承资产查询；无资产方向用 summary 中有区分度的关键词查询。has_more=true 时继续 next_cursor 或添加方向关键词收窄，完成后再 add_intent。仅本轮完整查询链有效；预载记录、旧历史和失败调用不算。此检查不替代资产审批，也不能保证语义去重。缺少读取工具或查询失败应说明限制并结束本轮，不得绕过。只有本任务当前允许的方向才可规划；手工模式停用 Planner。`

// Called by the trusted host during assembly, before any model/tool execution.
// Disabled search cannot be replaced by privileged automatic preloading.
func SetTaskContextReadAccess(ctx context.Context, p ContextAccess, searchEnabled bool) {
	if ts, _ := ctx.Value(taskToolSetContextKey{}).(*ToolSet); ts != nil {
		copy := p
		if !searchEnabled {
			copy.Local = nil
		}
		ts.plannerReadAccess = &copy
	}
}

// Refresh only the request copy. The initial snapshot is not an authorization
// credential, including when it survives compression or transcript restoration.
func (p assetContextProvider) refreshPlannerBootstrap(ctx context.Context, req llm.CompletionRequest) (llm.CompletionRequest, error) {
	if ri := RunInfoFrom(ctx); ri.AgentKey != "planner" || ri.TaskID != p.taskID {
		return req, nil
	}
	const open, close = "<planner_context>", "</planner_context>"
	req = cloneToolMessages(req)
	for mi, m := range req.Messages {
		for bi, b := range m.Content {
			if b.Type != llm.BlockText || !strings.Contains(b.Text, open) {
				continue
			}
			start := strings.Index(b.Text, open) + len(open)
			end := strings.Index(b.Text[start:], close)
			if end < 0 {
				req.Messages[mi].Content[bi].Text = b.Text[:start] + `{"unavailable":"引导记录不完整，请通过 context_search 重新读取"}` + close
				continue
			}
			end += start
			var value map[string]json.RawMessage
			if json.Unmarshal([]byte(b.Text[start:end]), &value) != nil {
				req.Messages[mi].Content[bi].Text = b.Text[:start] + `{"unavailable":"引导记录无法复核，请通过 context_search 重新读取"}` + b.Text[end:]
				continue
			}
			var records []db.PlannerContextRecord
			if json.Unmarshal(value["records"], &records) != nil {
				// Keep a standalone query error, but never carry an unverified
				// partial snapshot through history recovery as if it were readable.
				if len(value) != 1 || value["unavailable"] == nil {
					req.Messages[mi].Content[bi].Text = b.Text[:start] + `{"unavailable":"引导缺少可复核引用，请通过 context_search 重新读取"}` + b.Text[end:]
				}
				continue
			}
			ids := []int64{}
			for _, r := range records {
				if kind, id, err := parseContextRef(r.Ref); err == nil && kind == "node" {
					ids = append(ids, id)
				}
			}
			states, err := p.assets.WithReadContext(ctx).TaskNodeApprovalStates(p.taskID, ids)
			if err != nil {
				return req, err
			}
			kept := []db.PlannerContextRecord{}
			for _, r := range records {
				_, id, _ := parseContextRef(r.Ref)
				if states[id] == db.ApprovalApproved {
					kept = append(kept, r)
				}
			}
			value["records"], _ = json.Marshal(kept)
			value["counts_are_snapshot"] = json.RawMessage(`true`)
			if len(kept) != len(records) {
				delete(value, "counts")
				value["access_changed"] = json.RawMessage(`true`)
				value["has_more"] = json.RawMessage(`true`)
			}
			body, _ := json.Marshal(value)
			req.Messages[mi].Content[bi].Text = b.Text[:start] + string(body) + b.Text[end:]
		}
	}
	return req, nil
}

func (t *ToolSet) plannerBootstrap(ctx context.Context, triggers []TriggerEvent) string {
	const prefix = "\n\n【本轮图引导：按需检索，未展示不代表不存在】\n<planner_context>\n"
	const suffix = "\n</planner_context>"
	budget := plannerBootstrapRunes - utf8.RuneCountInString(prefix+suffix)
	policy := ContextAccess{Local: db.DefaultContextGrants("planner").Local}
	if t.plannerReadAccess != nil {
		policy = *t.plannerReadAccess
	}
	kinds := []string{}
	for _, k := range []string{"intent", "fact", "finding", "digest", "goal", "hint"} {
		if slices.Contains(policy.Local, k) {
			kinds = append(kinds, k)
		}
	}
	events := map[string]int{}
	seeds := []int64{}
	seen := map[int64]bool{}
	for i := len(triggers) - 1; i >= 0; i-- {
		e := triggers[i]
		kind := e.Kind
		if !slices.Contains([]string{"done", "finding", "goal", "goal_edited", "goal_deleted", "hint", "cancelled", "finding_deleted"}, kind) {
			kind = "change"
		}
		events[kind]++
		ids := append([]int64{}, e.NodeIDs...)
		if e.IntentID > 0 {
			ids = append(ids, e.IntentID)
		}
		for _, id := range ids {
			if id > 0 && !seen[id] {
				seen[id] = true
				if len(seeds) < 64 {
					seeds = append(seeds, id)
				}
			}
		}
	}
	reads := map[string]any{}
	for _, k := range kinds {
		reads[k] = map[string]any{"tool": "context_search", "args": map[string]any{"types": []string{k}, "limit": 10}}
	}
	out := map[string]any{"scope": "current_task_and_direct_sources", "events": events, "read": reads, "records": []db.PlannerContextRecord{}, "records_limit": 8, "records_are_related_sample": true,
		"note": "计数为当前授权可见记录；目标和战略提示未展示时按需检索，不能仅凭计数判定完成。无查询权限的类型不提供记录。"}
	if len(seen) > len(seeds) {
		out["events_truncated"] = true
	}
	// The original goal remains in the fixed system prompt; do not duplicate it.
	identity := map[string]any{"task_id": t.taskID}
	if t.ownerNode > 0 {
		identity["origin_ref"] = fmt.Sprintf("node:%d", t.ownerNode)
	}
	out["task"] = identity
	data, err := t.ts.PlannerBootstrap(ctx, kinds, seeds, len(triggers) == 0 && RunInfoFrom(ctx).Trigger == "heartbeat")
	if err != nil {
		out["unavailable"] = "图引导查询失败；不能视为零记录：" + err.Error()
		data.Records = nil
	} else {
		identity["description"] = data.TaskDescription
		if data.DescriptionOmitted {
			out["task_description_omitted"] = true
		}
		out["counts"] = data.Counts
		visibleCount := 0
		for _, c := range data.Counts {
			visibleCount += c.Count
		}
		out["has_more"] = data.HasMore || visibleCount > len(data.Records)
		if data.HasMore {
			out["related_records_truncated"] = true
		}
	}
	marshal := func() []byte { b, _ := json.Marshal(out); return b }
	for _, record := range data.Records {
		rows := out["records"].([]db.PlannerContextRecord)
		out["records"] = append(rows, record)
		if utf8.RuneCount(marshal()) > budget-128 {
			out["records"] = rows
			out["has_more"] = true
			break
		}
	}
	b := marshal()
	if utf8.RuneCount(b) > budget {
		b = []byte(`{"unavailable":"图引导元数据超过预算，请按资源类型使用 context_search；未展示不代表不存在"}`)
	}
	return prefix + string(b) + suffix
}
