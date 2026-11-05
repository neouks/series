package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/db"
)

const operationConstraintPrefix = "\n\n【操作约束（最高优先级，凌驾于下方一切探索/拓面启发式；每生成一条意图、每执行一个动作前都必须先自检是否违反，违反即不得进行）】："

type operationConstraintsKey struct{}
type operationConstraintsSource struct {
	store   *db.ExplorationStore
	enabled func(context.Context) (bool, error)
	owner   RunInfo
}

// Only the trusted run assembler installs this source; model parameters cannot
// change its task, role or injection setting. Read again for each model request.
func withOperationConstraints(ctx context.Context, ts *db.ExplorationStore, enabled func(context.Context) (bool, error)) context.Context {
	return context.WithValue(ctx, operationConstraintsKey{}, operationConstraintsSource{ts, enabled, RunInfoFrom(ctx)})
}

func (p assetContextProvider) refreshOperationConstraints(ctx context.Context, req llm.CompletionRequest) (llm.CompletionRequest, error) {
	source, ok := ctx.Value(operationConstraintsKey{}).(operationConstraintsSource)
	if !ok {
		return req, nil
	}
	ri := RunInfoFrom(ctx)
	if source.store == nil || ri.TaskID != p.taskID || ri.TaskID != source.owner.TaskID ||
		ri.ExplorationID != source.store.ID() || ri.ExplorationID != source.owner.ExplorationID ||
		ri.AgentKey != source.owner.AgentKey || (ri.AgentKey != "planner" && ri.AgentKey != "worker") {
		return req, fmt.Errorf("刷新操作约束失败：任务运行上下文不匹配")
	}
	req = withoutSystemSegments(req, func(s string) bool { return strings.HasPrefix(s, operationConstraintPrefix) })
	system := req.System
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	enabled := true
	if source.enabled != nil {
		var err error
		enabled, err = source.enabled(ctx)
		if err != nil {
			return req, fmt.Errorf("读取操作约束配置失败: %w", err)
		}
	}
	if enabled {
		block, err := constraintBlock(ctx, source.store)
		if err != nil {
			return req, err
		}
		if block != "" {
			system = append(system, block)
		}
	}
	req.System = system
	return req, nil
}

// constraintBlock renders this task's operation constraints (task_constraints) as a
// high-priority block appended to the planner/worker system prompt. allow/deny are
// grouped; empty string only when there are no nonempty constraints. The framing
// deliberately puts these ABOVE the exploration/expansion heuristics so a declared
// boundary wins the tug-of-war against "chase another entry surface".
func constraintBlock(ctx context.Context, ts *db.ExplorationStore) (string, error) {
	if ts == nil {
		return "", fmt.Errorf("读取操作约束失败：缺少任务探索上下文")
	}
	rows, err := ts.ListConstraintsContext(ctx)
	if err != nil {
		return "", fmt.Errorf("读取操作约束失败: %w", err)
	}
	return renderOperationConstraints(rows), nil
}

func renderOperationConstraints(rows []db.Constraint) string {
	var allow, deny []string
	seen := map[string]bool{}
	for _, c := range rows {
		text := strings.TrimSpace(c.Text)
		if text == "" {
			continue
		}
		// Remove exact duplicates only; differing kinds/wording remain intact.
		key := c.Kind + "\x00" + text
		if seen[key] {
			continue
		}
		seen[key] = true
		if c.Kind == "allow" {
			allow = append(allow, "- "+text)
		} else {
			deny = append(deny, "- "+text)
		}
	}
	if len(allow) == 0 && len(deny) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(operationConstraintPrefix)
	if len(allow) > 0 {
		b.WriteString("\n允许的操作：\n")
		b.WriteString(strings.Join(allow, "\n"))
	}
	if len(deny) > 0 {
		b.WriteString("\n禁止的操作：\n")
		b.WriteString(strings.Join(deny, "\n"))
	}
	b.WriteString("\n（发现约束之外的新目标/新端口/新主机，不等于获得授权：除非它落在上述允许范围内，否则记为 out-of-scope 事实并跳过，不得为其派生意图或执行动作。）")
	return b.String()
}
