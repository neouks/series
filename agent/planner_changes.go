package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/neouks/series/db"
)

type plannerChangeSnapshotKey struct{}

// WithPlannerChangeSnapshot is installed by the task engine before Plan. It
// never grants tool capabilities or overrides live asset authorization.
func WithPlannerChangeSnapshot(ctx context.Context, snap db.PlannerEventSnapshot) context.Context {
	return context.WithValue(ctx, plannerChangeSnapshotKey{}, snap)
}

func (t *ToolSet) plannerReadKinds(ctx context.Context) []string {
	policy := ContextAccess{Local: db.DefaultContextGrants("planner").Local}
	if t.plannerReadAccess != nil {
		policy = *t.plannerReadAccess
	}
	kinds := []string{}
	for _, kind := range []string{"intent", "fact", "finding", "digest", "goal", "hint"} {
		if slices.Contains(policy.Local, kind) && contextAllows(ctx, kind, false) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

func (t *ToolSet) plannerChangeSnapshot(ctx context.Context) (db.PlannerEventSnapshot, error) {
	snap, ok := ctx.Value(plannerChangeSnapshotKey{}).(db.PlannerEventSnapshot)
	ri := RunInfoFrom(ctx)
	if !ok || t.ts == nil || snap.ExplorationID != t.ts.ID() || ri.TaskID != t.taskID ||
		ri.ExplorationID != t.ts.ID() || ri.AgentKey != "planner" {
		return snap, fmt.Errorf("planner_changes仅允许Planner读取当前任务的本轮变动")
	}
	return snap, nil
}

func (t *ToolSet) plannerChangeTriggers(ctx context.Context, triggers []TriggerEvent) ([]TriggerEvent, error) {
	if _, ok := ctx.Value(plannerChangeSnapshotKey{}).(db.PlannerEventSnapshot); !ok {
		return triggers, nil // direct, task-bound Plan calls remain compatible.
	}
	snap, err := t.plannerChangeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := t.ts.PlannerChangePage(ctx, snap, t.plannerReadKinds(ctx), 0, 64)
	if err != nil {
		return nil, fmt.Errorf("读取Planner持久变动失败: %w", err)
	}
	more := len(rows) > 64
	rows = rows[:min(64, len(rows))]
	out := append([]TriggerEvent(nil), triggers...)
	for _, row := range rows {
		kind := row.Kind
		if kind == "intent" && slices.Contains([]string{"done", "blocked", "exhausted", "stopped"}, row.State) {
			kind = "done"
		}
		out = append(out, TriggerEvent{Kind: kind, NodeIDs: []int64{row.ID}})
	}
	if more || (snap.Pending() && len(rows) == 0 && len(out) == 0) {
		out = append(out, TriggerEvent{Kind: "change", Truncated: more})
	}
	return out, nil
}

func (t *ToolSet) readPlannerChanges(ctx context.Context, task string, item contextRead, budget int) (map[string]any, error) {
	id, err := strconv.ParseInt(item.Ref[len("planner_changes:"):], 10, 64)
	if err != nil || id <= 0 || id != t.taskID || (task != "" && task != strconv.FormatInt(id, 10)) {
		return nil, fmt.Errorf("planner_changes不支持跨任务读取")
	}
	snap, err := t.plannerChangeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if item.View != "" && item.View != "detail" || item.Field != "" || item.Index != 0 || item.Offset != 0 || item.Before < 0 {
		return nil, fmt.Errorf("planner_changes仅支持detail及before续页")
	}
	if _, err = t.contextTarget(ctx, "", "detail"); err != nil {
		return nil, err
	}
	limit := min(20, max(1, budget/600))
	rows, err := t.ts.PlannerChangePage(ctx, snap, t.plannerReadKinds(ctx), item.Before, limit)
	if err != nil {
		return nil, err
	}
	selected := []db.PlannerChange{}
	for _, row := range rows {
		candidate := append(append([]db.PlannerChange{}, selected...), row)
		raw, _ := json.Marshal(candidate)
		if len([]rune(string(raw)))+350 > budget || len(candidate) > limit {
			break
		}
		selected = candidate
	}
	more := len(selected) < len(rows)
	if len(selected) == 0 && len(rows) > 0 {
		return nil, fmt.Errorf("输出预算不足，请增大max_chars后读取变动元数据")
	}
	out := map[string]any{"ref": item.Ref, "items": selected, "has_more": more, "selection_is_snapshot": true, "content_is_current": true, "is_navigation": true}
	if more {
		out["next"] = map[string]any{"items": []contextRead{{Ref: item.Ref, Before: selected[len(selected)-1].ID}}}
	}
	return out, nil
}
