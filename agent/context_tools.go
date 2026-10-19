package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

// ContextAccess is host-owned; neither model parameters nor editable prose can
// enlarge it. Separate grants preserve the old local/cross-task bindings.
type ContextAccess struct {
	Local []string `json:"local"`
	Cross []string `json:"cross"`
}
type contextAccessKey struct{}

func WithContextAccess(ctx context.Context, p ContextAccess) context.Context {
	return context.WithValue(ctx, contextAccessKey{}, p)
}
func contextAllows(ctx context.Context, cap string, cross bool) bool {
	p, ok := ctx.Value(contextAccessKey{}).(ContextAccess)
	if !ok {
		return !cross
	}
	grants := p.Local
	if cross {
		grants = p.Cross
	}
	return slices.Contains(grants, cap)
}

func (t *ToolSet) workerReadAllowed(ctx context.Context) bool {
	ri := RunInfoFrom(ctx)
	return t.workerExecution && t.taskID > 0 && ri.TaskID == t.taskID && ri.AgentKey == "worker" && ri.IntentID > 0
}

// ResolveContextTask is supplied by the host. Only a trusted cross-task grant
// can reach it; task-bound roles cannot switch tasks through a forged parameter.
var ResolveContextTask func(context.Context, string) (*ToolSet, error)

func (t *ToolSet) contextTarget(ctx context.Context, task string, cap string) (*ToolSet, error) {
	if t == nil {
		return nil, fmt.Errorf("缺少任务探索上下文")
	}
	cross := t.contextCrossTask(task)
	if !contextAllows(ctx, cap, cross) {
		if cross {
			return nil, fmt.Errorf("当前角色没有跨任务 %s 读取权限", cap)
		}
		return nil, fmt.Errorf("当前角色没有 %s 读取权限", cap)
	}
	if !cross {
		if t.ts == nil {
			return nil, fmt.Errorf("缺少任务探索上下文")
		}
		copy := *t
		copy.workerExecution = t.workerReadAllowed(ctx)
		if copy.workerExecution {
			copy.ts = t.ts.WithWorkerRead()
		}
		if copy.workerExecution != t.workerExecution || copy.ts != t.ts {
			return &copy, nil
		}
		return t, nil
	}
	if ResolveContextTask == nil {
		return nil, fmt.Errorf("跨任务读取不可用")
	}
	return ResolveContextTask(ctx, task)
}

var ContextTypes = []string{"fact", "finding", "intent", "goal", "hint", "digest", "activity"}

func (t *ToolSet) ContextSearchTool() actool.CoreTool { return t.contextSearch() }
func (t *ToolSet) ContextGetTool() actool.CoreTool    { return t.contextGet() }

type contextSearchInput struct {
	TaskID   contextTaskID `json:"task_id"`
	Types    []string      `json:"types"`
	Q        string        `json:"q"`
	State    string        `json:"state"`
	Severity string        `json:"severity"`
	AssetID  int64         `json:"asset_id"`
	IntentID int64         `json:"intent_id"`
	Limit    int           `json:"limit"`
	Cursor   string        `json:"cursor"`
}
type readCursor struct {
	Signature string `json:"s"`
	Upper     int64  `json:"u"`
	Before    int64  `json:"b"`
}

func encodeReadCursor(c readCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeReadCursor(s, signature string) (readCursor, error) {
	var c readCursor
	b, e := base64.RawURLEncoding.DecodeString(s)
	if e == nil {
		e = json.Unmarshal(b, &c)
	}
	if e != nil || c.Signature != signature || c.Upper < 0 || c.Before <= 0 || c.Before > c.Upper {
		return c, fmt.Errorf("游标无效或查询条件已改变")
	}
	return c, nil
}
func querySignature(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}
func (t *ToolSet) contextSearch() actool.CoreTool {
	return readTool("context_search", "按类型检索授权可见的任务上下文摘要和正文，返回命中片段、match_offset及node:ID/activity:ID；read可直接传给context_get，从命中附近读取。默认查询有权限的 fact/finding/intent。activity 只含 Worker 执行记录，需显式选择，可混合查询；混合结果按登记时间倒序，单类按ID倒序。默认10条、最多50条，输出预算可能缩小页长；has_more/next_cursor续页。", obj(map[string]any{
		"task_id": contextTaskSchema(), "types": map[string]any{"type": "array", "description": "选择当前角色可用类型；省略时从 fact/finding/intent 中取有权限的类型，activity须显式选择", "items": map[string]any{"type": "string", "enum": ContextTypes}},
		"q": str("不区分大小写的字面关键词，最多200字符；查摘要及正文（漏洞含报告），空值列出记录，不支持正则"), "asset_id": idp("关联资产ID"), "intent_id": idp("意图节点ID：查询该意图及其产出；activity查询该Worker记录"), "state": str("节点状态；activity按所属意图状态筛选，如open/running/done/blocked/exhausted/stopped"), "severity": str("payload.severity精确筛选，如critical/high/medium/low；其他不含该字段的记录也被排除"), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 10}, "cursor": str("保留原查询条件，传入next_cursor；新消息不插入当前分页"),
	}), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var a contextSearchInput
		if err := decodeToolInput(raw, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		a.Q = strings.TrimSpace(a.Q)
		if len([]rune(a.Q)) > 200 || a.AssetID < 0 || a.IntentID < 0 {
			return actool.Errorf("关键词最多200字符，ID不得为负"), nil
		}
		if a.Limit == 0 {
			a.Limit = 10
		}
		if a.Limit < 1 || a.Limit > 50 {
			return actool.Errorf("limit 必须为1..50"), nil
		}
		a.Limit = min(a.Limit, max(1, (readOutputBudget(ctx)-1024)/1800))
		cross := t.contextCrossTask(string(a.TaskID))
		if len(a.Types) == 0 {
			for _, k := range []string{"fact", "finding", "intent"} {
				if contextAllows(ctx, k, cross) {
					a.Types = append(a.Types, k)
				}
			}
		}
		if len(a.Types) == 0 {
			return actool.Errorf("没有可用的检索类型"), nil
		}
		slices.Sort(a.Types)
		a.Types = slices.Compact(a.Types)
		var target *ToolSet
		for _, kind := range a.Types {
			if !slices.Contains(ContextTypes, kind) {
				return actool.Errorf("无效资源类型"), nil
			}
			cap := kind
			if kind == "activity" && a.IntentID == 0 {
				cap = "activity_all"
			}
			var err error
			target, err = t.contextTarget(ctx, string(a.TaskID), cap)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
		}
		signatureInput := a
		signatureInput.Cursor = ""
		signatureInput.Limit = 0
		signatureInput.TaskID = contextTaskID(strconv.FormatInt(target.taskID, 10))
		signature := querySignature(signatureInput)
		if len(a.Types) > 1 && slices.Contains(a.Types, "activity") {
			return target.searchMixedContext(ctx, a, signature, cross)
		}
		var cursor readCursor
		var err error
		if a.Cursor != "" {
			cursor, err = decodeReadCursor(a.Cursor, signature)
		} else {
			cursor.Signature = signature
			cursor.Upper = math.MaxInt64
		}
		if err != nil {
			return actool.Errorf("查询失败: " + err.Error()), nil
		}
		hits, err := target.ts.SearchContext(ctx, db.ContextSearch{Types: a.Types, Query: a.Q, State: a.State, Severity: a.Severity, AssetID: a.AssetID, IntentID: a.IntentID, Before: cursor.Before, Upper: cursor.Upper, Limit: a.Limit})
		if err != nil {
			return actool.Errorf("查询失败: " + err.Error()), nil
		}
		more := len(hits) > a.Limit
		if more {
			hits = hits[:a.Limit]
		}
		t.addContextHitReads(ctx, a, hits, cross)
		out := map[string]any{"items": hits, "has_more": more}
		if more {
			if a.Cursor == "" {
				cursor.Upper = hits[0].ID
			}
			cursor.Before = hits[len(hits)-1].ID
			out["next_cursor"] = encodeReadCursor(cursor)
		}
		return contextJSON(ctx, out)
	})
}

// Keep the original query scope, not source_task_id: inherited records are read
// through the current task's permissions, never an implicit cross-task grant.
func (t *ToolSet) addContextHitReads(ctx context.Context, a contextSearchInput, hits []db.ContextHit, cross bool) {
	for i := range hits {
		h := &hits[i]
		cap := "detail"
		if h.Kind == "activity" {
			cap = "activity_detail"
		}
		if !contextAllows(ctx, cap, cross) {
			continue
		}
		item := contextRead{Ref: h.Ref, Field: h.Field}
		if h.MatchOffset != nil {
			item.Offset = max(0, *h.MatchOffset-60)
		}
		h.Read = map[string]any{"items": []contextRead{item}}
		if a.TaskID != "" {
			h.Read["task_id"] = a.TaskID
		}
	}
}

type contextRead struct {
	MaxChars *int   `json:"max_chars,omitempty"`
	Ref      string `json:"ref"`
	View     string `json:"view,omitempty"`
	Field    string `json:"field,omitempty"`
	Index    int    `json:"index,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	Before   int64  `json:"before,omitempty"`
}

func parseContextRef(ref string) (string, int64, error) {
	parts := strings.Split(ref, ":")
	if len(parts) != 2 || (parts[0] != "node" && parts[0] != "activity") {
		return "", 0, fmt.Errorf("ref 必须为 node:ID 或 activity:ID")
	}
	id, e := strconv.ParseInt(parts[1], 10, 64)
	if e != nil || id <= 0 {
		return "", 0, fmt.Errorf("ref ID 必须为正整数")
	}
	return parts[0], id, nil
}
func (t *ToolSet) contextGet() actool.CoreTool {
	item := obj(map[string]any{"ref": str("node:探索节点ID 或 activity:活动ID；不能使用独立finding_id或资产ID"), "view": map[string]any{"type": "string", "description": "默认detail；output/parents仅意图节点，members仅摘要节点；activity仅detail", "enum": []string{"detail", "output", "members", "parents"}}, "field": str("detail的JSON Pointer；节点从payload根取，如/evidence、/report；activity可取/detail或/summary；省略先看字段目录"), "index": intp("detail集合的next_index，默认0"), "offset": intp("detail/output文本的next_offset，按Unicode字符计，默认0"), "before": intp("members/parents的next_before；不能与field/index/offset组合"), "max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 24000, "description": "该项正文上限；仍受顶层总预算和工具输出上限约束"}}, "ref")
	return readTool("context_get", "批量读取1..5个引用；view选择正文、意图输出、上游事实或摘要成员。每项独立返回错误和可直接调用的next；output区分最终结果与中途进度。顶层max_chars为总预算（默认8000、最多24000），各项均分，每项max_chars只能进一步收紧；工具输出上限仍生效。", obj(map[string]any{"task_id": contextTaskSchema(), "items": map[string]any{"type": "array", "minItems": 1, "maxItems": 5, "items": item}, "max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 24000, "default": 8000}}, "items"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var a struct {
			TaskID   contextTaskID `json:"task_id"`
			Items    []contextRead `json:"items"`
			MaxChars int           `json:"max_chars"`
		}
		if e := decodeToolInput(raw, &a); e != nil {
			return actool.Errorf(e.Error()), nil
		}
		if a.MaxChars == 0 {
			a.MaxChars = 8000
		}
		if len(a.Items) < 1 || len(a.Items) > 5 || a.MaxChars < 1 || a.MaxChars > 24000 {
			return actool.Errorf("items 须1..5项，max_chars 须1..24000"), nil
		}
		a.MaxChars = readContentBudget(ctx, a.MaxChars, len(a.Items))
		rows := []map[string]any{}
		for i, item := range a.Items {
			budget := a.MaxChars / len(a.Items)
			if i < a.MaxChars%len(a.Items) {
				budget++
			}
			if budget == 0 {
				rows = append(rows, map[string]any{"ref": item.Ref, "error": "总正文预算不足；请减少 items 数量或提高顶层 max_chars"})
				continue
			}
			row, e := t.readContextItem(ctx, string(a.TaskID), item, budget)
			if e != nil {
				row = map[string]any{"ref": item.Ref, "error": e.Error()}
			}
			rows = append(rows, row)
		}
		return contextJSON(ctx, map[string]any{"items": rows})
	})
}
func (t *ToolSet) readContextItem(ctx context.Context, task string, item contextRead, budget int) (map[string]any, error) {
	if item.MaxChars != nil {
		if *item.MaxChars < 1 || *item.MaxChars > 24000 {
			return nil, fmt.Errorf("items[].max_chars 须1..24000")
		}
		budget = min(budget, *item.MaxChars)
	}
	kind, id, e := parseContextRef(item.Ref)
	if e != nil {
		return nil, e
	}
	if item.Index < 0 || item.Offset < 0 || item.Before < 0 {
		return nil, fmt.Errorf("续读位置不得为负")
	}
	if item.View == "" {
		item.View = "detail"
	}
	if item.View == "output" && (item.Field != "" || item.Index != 0 || item.Before != 0) {
		return nil, fmt.Errorf("output 仅支持 offset")
	}
	if (item.View == "members" || item.View == "parents") && (item.Field != "" || item.Index != 0 || item.Offset != 0) {
		return nil, fmt.Errorf("%s 仅支持 before", item.View)
	}
	if item.View == "detail" && item.Before != 0 {
		return nil, fmt.Errorf("detail 不支持 before")
	}
	cap := "detail"
	if kind == "activity" {
		cap = "activity_detail"
	} else if item.View != "detail" && item.View != "parents" {
		cap = item.View
	}
	target, e := t.contextTarget(ctx, task, cap)
	if e != nil {
		return nil, e
	}
	var out map[string]any
	if kind == "activity" {
		if item.View != "detail" || item.Index != 0 || item.Before != 0 {
			return nil, fmt.Errorf("activity 仅支持 detail/field/offset")
		}
		out, e = target.ts.ContextActivity(ctx, id, item.Offset, budget, item.Field)
	} else if item.View == "output" {
		out, e = target.ts.ContextOutput(ctx, id, item.Offset, budget)
	} else {
		if !slices.Contains([]string{"detail", "members", "parents"}, item.View) {
			return nil, fmt.Errorf("无效 view")
		}
		field, index, offset := item.Field, item.Index, item.Offset
		if item.View != "detail" {
			field = "/summary"
			index = 0
			offset = 0
		}
		out, e = target.ts.ContextNode(ctx, id, field, index, offset, budget)
		if e == nil && item.View == "parents" {
			if out["kind"] != "intent" {
				return nil, fmt.Errorf("parents 仅适用于意图")
			}
			limit := min(20, max(1, budget/1000))
			parents, err := target.ts.ContextParents(ctx, id, item.Before, limit, budget/limit)
			if err != nil {
				return nil, err
			}
			more := len(parents) > limit
			if more {
				parents = parents[:limit]
				out["next_before"] = parents[len(parents)-1].ID
			}
			delete(out, "payload")
			out["parents"], out["has_more"] = parents, more
		} else if e == nil && item.View == "members" {
			if out["kind"] != "digest" {
				return nil, fmt.Errorf("members 仅适用于摘要")
			}
			members, err := target.ts.ContextDigestMembers(ctx, id, item.Before, min(10, max(1, budget/500)), max(1, min(240, budget)))
			if err != nil {
				return nil, err
			}

			memberLimit := min(10, max(1, budget/500))
			more := len(members) > memberLimit
			if more {
				members = members[:memberLimit]
			}
			list := []map[string]any{}
			for _, n := range members {
				row := compactFact(n)
				row["source_task_id"] = out["source_task_id"]
				row["inherited"] = out["inherited"]
				row["node_id"] = n.ID
				row["ref"] = fmt.Sprintf("node:%d", n.ID)
				list = append(list, row)
			}
			delete(out, "payload")
			out["members"] = list
			out["has_more"] = more
			if more {
				out["next_before"] = members[len(members)-1].ID
			}
		}
	}
	if e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return nil, fmt.Errorf("记录不可用")
		}
		return nil, fmt.Errorf("读取失败: %w", e)
	}
	out["ref"] = item.Ref
	positions := out
	if payload, ok := out["payload"].(map[string]any); ok {
		positions = payload
	}
	next := item
	more := false
	if v, ok := positions["next_offset"]; ok && v != nil {
		if n, ok := structuredInt64(v); ok {
			next.Offset = int(n)
			more = true
		}
	}
	if v, ok := structuredInt64(positions["next_index"]); ok {
		next.Index = int(v)
		more = true
	}
	if v, ok := positions["next_before"].(int64); ok {
		next.Before = v
		more = true
	}
	if more {
		continuation := map[string]any{"items": []contextRead{next}}
		if task != "" {
			continuation["task_id"] = task
		}
		out["next"] = continuation
	}
	return out, nil
}
