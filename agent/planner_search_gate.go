package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// One ledger per Plan invocation, shared by its task-bound direct/deferred tools.
// Neither transcript replay nor JSON parameters can construct this state.
type plannerSearchLedger struct {
	mu       sync.Mutex
	queries  map[string]plannerSearchReceipt
	sequence uint64
}
type plannerSearchReceipt struct {
	AssetID  int64
	Query    string
	Next     string
	Complete bool
	sequence uint64
}
type plannerSearchRequired struct {
	Reads       []map[string]any `json:"reads"`
	Unavailable string           `json:"unavailable,omitempty"`
}

// Freeze proof at submission entry, so an add_intent racing an unfinished read
// cannot gain permission merely by waiting behind the database queue lock.
func (l *plannerSearchLedger) snapshot() *plannerSearchLedger {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	copy := &plannerSearchLedger{queries: map[string]plannerSearchReceipt{}}
	for k, r := range l.queries {
		copy.queries[k] = r
	}
	return copy
}

func (e *plannerSearchRequired) Error() string {
	if e.Unavailable != "" {
		return e.Unavailable
	}
	return "规划前需完成本轮相关意图查询；使用返回的 reads 搜索，不限定 state/intent_id；has_more 时继续分页或按方向收窄，未查完不代表不存在"
}

func (t *ToolSet) recordPlannerSearch(ctx context.Context, q contextSearchInput, signature string, out map[string]any) {
	ri := RunInfoFrom(ctx)
	if t.plannerSearches == nil || ri.AgentKey != "planner" || ri.TaskID != t.taskID || t.contextCrossTask(string(q.TaskID)) ||
		!slices.Contains(q.Types, "intent") || q.State != "" || q.IntentID != 0 || q.Severity != "" {
		return
	}
	l := t.plannerSearches
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queries == nil {
		l.queries = map[string]plannerSearchReceipt{}
	}
	r, exists := l.queries[signature]
	// Accept only an actual chain beginning at the first page. Opaque cursors
	// from a prior round or fabricated last-page cursors are not search proof.
	if q.Cursor != "" && (!exists || r.Complete || r.Next != q.Cursor) {
		return
	}
	next, _ := out["next_cursor"].(string)
	more, _ := out["has_more"].(bool)
	if len(l.queries) >= 128 && !exists {
		var oldest string
		for key, value := range l.queries {
			if oldest == "" || value.sequence < l.queries[oldest].sequence {
				oldest = key
			}
		}
		delete(l.queries, oldest)
	}
	l.sequence++
	l.queries[signature] = plannerSearchReceipt{AssetID: q.AssetID, Query: strings.ToLower(strings.TrimSpace(q.Q)), Next: next, Complete: !more, sequence: l.sequence}
}

func (t *ToolSet) checkPlannerSearch(ctx context.Context, summary string, assets []int64) error {
	if t.plannerReadAccess != nil && !slices.Contains(t.plannerReadAccess.Local, "intent") {
		return &plannerSearchRequired{Unavailable: "缺少已启用、已绑定的 context_search 或本任务 intent 读取权限；无法完成规划前查询，请修复工具配置", Reads: []map[string]any{}}
	}
	ri := RunInfoFrom(ctx)
	if ri.AgentKey != "planner" || ri.TaskID != t.taskID || t.plannerSearches == nil {
		return &plannerSearchRequired{Reads: plannerSearchReads(summary, assets)}
	}
	text := strings.ToLower(strings.TrimSpace(summary))
	l := t.plannerSearches
	l.mu.Lock()
	defer l.mu.Unlock()
	covered := func(asset int64) bool {
		for _, r := range l.queries {
			if !r.Complete || r.AssetID != asset {
				continue
			}
			if r.Query == "" {
				if asset > 0 {
					return true
				}
				continue
			}
			if strings.Contains(text, r.Query) {
				return true
			}
		}
		return false
	}
	missing := []int64{}
	for _, id := range assets {
		if !covered(id) {
			missing = append(missing, id)
		}
	}
	if len(assets) == 0 && !covered(0) {
		return &plannerSearchRequired{Reads: plannerSearchReads(summary, nil)}
	}
	if len(missing) > 0 {
		return &plannerSearchRequired{Reads: plannerSearchReads(summary, missing)}
	}
	return nil
}

func plannerSearchReads(summary string, assets []int64) []map[string]any {
	reads := []map[string]any{}
	if len(assets) == 0 {
		q := []rune(strings.TrimSpace(summary))
		if len(q) > 200 {
			q = q[:200]
		}
		return []map[string]any{{"tool": "context_search", "args": map[string]any{"types": []string{"intent"}, "q": string(q), "limit": 10}, "note": "按方向关键词查询；可将 q 缩短为 summary 中有区分度的短语"}}
	}
	for _, id := range assets {
		reads = append(reads, map[string]any{"tool": "context_search", "args": map[string]any{"types": []string{"intent"}, "asset_id": id, "limit": 10}, "note": fmt.Sprintf("检查资产 %d 的已有方向；需要时添加 summary 中的关键词缩小范围", id)})
	}
	return reads
}
