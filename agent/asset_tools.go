package agent

import (
	"context"
	"encoding/json"
	actool "github.com/Autumn-27/norma/tool"
	"slices"
	"strings"
	"time"
)

func (t *ToolSet) assetSearch() actool.CoreTool {
	return readTool("asset_search", "检索当前角色可见资产摘要；详情用asset_get。通常必须提供非空dsl，不能只传type/limit；有untested权限时可用untested=true查询任务覆盖范围内未测试资产。默认10条、最多50条；保留筛选条件，以next_offset作为offset续页。", obj(map[string]any{"dsl": str("非空筛选表达式，如 port==443 AND technology=nginx；支持domain/ip/url/port/technology、AND/OR、=模糊、==精确；untested=true时可省略"), "type": str("独立资产类型参数，如ip/subdomain；不要写成dsl中的type=...，不能单独代替dsl"), "untested": map[string]any{"type": "boolean"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 10}, "offset": intp("next_offset 续页")}), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var a struct {
			DSL      string `json:"dsl"`
			Type     string `json:"type"`
			Untested bool   `json:"untested"`
			Limit    int    `json:"limit"`
			Offset   int    `json:"offset"`
		}
		if e := decodeToolInput(raw, &a); e != nil {
			return actool.Errorf(e.Error()), nil
		}
		if a.Limit == 0 {
			a.Limit = 10
		}
		if a.Limit < 1 || a.Limit > 50 || a.Offset < 0 {
			return actool.Errorf("limit 须1..50，offset不得为负"), nil
		}
		if !a.Untested && strings.TrimSpace(a.DSL) == "" {
			return actool.Errorf("asset_search 需要 dsl 条件；已知ID用 asset_get，未测试资产用 untested=true"), nil
		}
		a.Limit = min(a.Limit, max(1, (readOutputBudget(ctx)-1024)/1800))
		if !contextAllows(ctx, "assets", false) && !a.Untested {
			return actool.Errorf("未授予资产搜索权限"), nil
		}
		if a.Untested {
			if !contextAllows(ctx, "untested", false) {
				return actool.Errorf("未授予未测试资产读取权限"), nil
			}
			if t.as == nil || t.taskID <= 0 || t.coverageDisabled {
				return actool.Errorf("未测试查询需要启用覆盖度的任务"), nil
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			store := t.as.WithReadContext(ctx).WithToolReadFields(false, nil)
			if t.workerReadAllowed(ctx) {
				store = store.WithWorkerRead()
			}
			assets, e := store.SearchUntestedToolAssets(t.taskID, a.DSL, a.Type, a.Limit+1, a.Offset)
			if e != nil {
				return actool.Errorf(e.Error()), nil
			}
			more := len(assets) > a.Limit
			if more {
				assets = assets[:a.Limit]
			}
			rows := []map[string]any{}
			for _, asset := range assets {
				r := assetSummary(asset, true)
				r["asset_id"] = asset.ID
				rows = append(rows, r)
			}
			out := map[string]any{"assets": rows, "has_more": more}
			if more {
				out["next_offset"] = a.Offset + len(rows)
			}
			return jsonResult(out)
		}
		in, _ := json.Marshal(map[string]any{"dsl": a.DSL, "type": a.Type, "limit": a.Limit, "offset": a.Offset})
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		copy := *t
		copy.workerExecution = t.workerReadAllowed(ctx)
		return copy.listAssets().Call(ctx, in, nil)
	})
}
func (t *ToolSet) assetGet() actool.CoreTool {
	return readTool("asset_get", "按ids读取1..5个资产，重复ID去重。默认身份与指纹；凭证需fields显式包含auth。field从字段组根取，如/identity/domain；省略先看字段目录。max_chars是整批总预算（默认8000、最多24000），工具输出上限仍生效；按每项next续读。", obj(map[string]any{
		"ids":    map[string]any{"type": "array", "minItems": 1, "maxItems": 5, "items": map[string]any{"type": "integer", "minimum": 1}},
		"fields": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"identity", "fingerprint", "dns", "params", "auth", "extra"}}},
		"field":  str("JSON Pointer"), "index": intp("集合续页"), "offset": intp("文本续读"), "max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 24000, "default": 8000},
	}, "ids"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var a struct {
			IDs    []int64  `json:"ids"`
			Fields []string `json:"fields"`
			Field  string   `json:"field"`
			Index  int      `json:"index"`
			detailWindow
		}
		if e := decodeToolInput(raw, &a); e != nil {
			return actool.Errorf(e.Error()), nil
		}
		if !contextAllows(ctx, "assets", false) {
			return actool.Errorf("未授予资产读取权限"), nil
		}
		if len(a.IDs) < 1 || len(a.IDs) > 5 {
			return actool.Errorf("ids 须1..5项"), nil
		}
		if e := a.validate(); e != nil {
			return actool.Errorf(e.Error()), nil
		}
		if len(a.Fields) == 0 {
			a.Fields = []string{"identity", "fingerprint"}
		}
		if a.Index < 0 {
			return actool.Errorf("index 不得为负"), nil
		}
		for _, id := range a.IDs {
			if id <= 0 {
				return actool.Errorf("ids 必须为正整数"), nil
			}
		}
		// Duplicate selectors do not consume another share of the total budget.
		seen := map[int64]bool{}
		ids := make([]int64, 0, len(a.IDs))
		for _, id := range a.IDs {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		a.IDs = ids
		a.MaxChars = readContentBudget(ctx, a.MaxChars, len(ids))
		for _, group := range a.Fields {
			if !slices.Contains([]string{"identity", "fingerprint", "dns", "params", "auth", "extra"}, group) {
				return actool.Errorf("未知字段组"), nil
			}
		}
		if t.as == nil {
			return actool.Errorf("资产存储不可用"), nil
		}
		store := t.as
		if t.workerReadAllowed(ctx) {
			store = store.WithWorkerRead()
		}
		// Do not multiply tiny total budgets by forcing every item to one char.
		queryIDs := ids[:min(len(ids), a.MaxChars)]
		rows, e := store.ContextAssets(ctx, t.taskID, queryIDs, a.Fields, a.Field, a.Index, a.Offset, a.MaxChars/len(queryIDs))
		if e != nil {
			return actool.Errorf("资产查询失败: " + e.Error()), nil
		}
		found := map[int64]bool{}
		for _, row := range rows {
			id, ok := structuredInt64(row["asset_id"])
			if !ok || id <= 0 {
				return actool.Errorf("资产查询返回无效 ID"), nil
			}
			found[id] = true
			if detail, ok := row["details"].(map[string]any); ok {
				next := map[string]any{"ids": []int64{id}, "fields": a.Fields, "field": a.Field, "index": a.Index, "offset": a.Offset, "max_chars": a.MaxChars}
				more := false
				for _, position := range []string{"offset", "index"} {
					if v := detail["next_"+position]; v != nil {
						next[position] = v
						more = true
					}
				}
				if more {
					row["next"] = next
				}
			}
		}
		for i, id := range a.IDs {
			if !found[id] {
				reason := "资产不可用"
				if i >= len(queryIDs) {
					reason = "总正文预算不足；请减少 ids 数量或提高 max_chars"
				}
				rows = append(rows, map[string]any{"asset_id": id, "error": reason})
				found[id] = true
			}
		}
		return contextJSON(ctx, map[string]any{"assets": rows})

	})
}
