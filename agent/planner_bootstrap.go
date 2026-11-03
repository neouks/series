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
【Planner 规划与下发流程，替代旧版查询前置要求】依据任务目标、本轮事件和已有上下文判断下一步。信息足够时直接调用 add_intent，无需先查询已有意图、资产或探索图；仅在信息不足、资产身份不明或需要核对历史进度时按需检索。仅监督或等待结果时复用已有信息，不增加固定查询。
结合已知的排队、运行和历史方向避免重复；数据库仍执行去重。不要把旧提示词或历史中的先查后建要求作为下发前置步骤。检索工具被禁用、未查询或尚有下一页，均不单独阻止合法意图提交；权限和执行约束照常检查。
本轮图引导是授权计数、事件和少量相关摘要，不是完整探索图，未展示不代表不存在。需要补全时使用 context_search(types/q/asset_id/cursor)、context_get(items/ref/view/field)；资产身份不明时使用 asset_search(dsl/type/offset)，详细字段用 asset_get(ids)。已知资产 ID 可直接用于 add_intent.asset_ids，不必重新搜索。has_more 只表示未展示完，按需收窄或续页，不要求遍历全图。查询失败时说明信息缺口，不原样重复失败调用。
资产审批、工具绑定、任务隔离、用户取消和禁报约束继续生效；手工模式停用 Planner。摘要仅用于导航，目标达成和关键判断必须依据原始证据；区分observed/inferred/failed/unverified，不把未展示或查询失败当成不存在。`

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
			var previousAccess string
			_ = json.Unmarshal(value["access_revision"], &previousAccess)
			if previousAccess != "" {
				if ts, _ := ctx.Value(taskToolSetContextKey{}).(*ToolSet); ts != nil && ts.ts != nil && ts.taskID == p.taskID {
					current, e := ts.ts.GraphAccessRevision(ctx)
					if e != nil {
						return req, e
					}
					if current != previousAccess {
						delete(value, "counts")
						value["counts_access_changed"] = json.RawMessage(`true`)
					}
				}
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
		out["graph_revision"] = data.Revision
		visibleCount := 0
		for _, c := range data.Counts {
			visibleCount += c.Count
		}
		out["has_more"] = data.HasMore || visibleCount > len(data.Records)
		if data.HasMore {
			out["related_records_truncated"] = true
		}
	}
	if data.AccessRevision != "" {
		out["access_revision"] = data.AccessRevision
	} else {
		delete(out, "counts")
		out["counts_unavailable"] = "审批范围版本读取失败"
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
