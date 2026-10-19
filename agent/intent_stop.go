package agent

import (
	"context"
	"encoding/json"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

// IntentStopper is task-scoped and returns only after execution has stopped and
// the final state is persisted. User cancellation and planner termination use
// different host callbacks so planner decisions never acquire the user lock.
type IntentStopper func(ctx context.Context, taskID, intentID int64, reason string) (IntentStopResult, error)

type IntentStopResult struct {
	ID              int64  `json:"id"`
	State           string `json:"state"`
	Reason          string `json:"reason,omitempty"`
	CancelledByUser bool   `json:"cancelled_by_user"`
}

func (t *ToolSet) cancelIntentTool() actool.CoreTool {
	return t.intentStopTool("cancel_intent", "mainagent", "按用户明确要求取消本任务的一条意图：退出等待队列或停止运行中的 Worker，也可取消已暂停意图。保留意图、主会话与已有产出，记录取消原因并通知 Planner；仅用户手动重新开启后可执行。成功返回表示执行已停止且状态已落库；错误或超时不能宣称取消成功。", t.cancelIntent)
}

func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.intentStopTool("kill_work", "planner", "终止本任务一条等待运行或正在运行的意图，用于叫停跑偏、重复或无价值的探索。先用 context_get 的 output 视图了解进度。成功后状态为 stopped，保留已有产出，不再自动领取；不会设置用户取消锁。已暂停意图由用户控制。成功返回表示执行已停止且状态已落库；错误或超时不能宣称终止成功。", t.killWork)
}

func (t *ToolSet) intentStopTool(name, role, description string, stop IntentStopper) actool.CoreTool {
	return t.writeExpTool(name, description, obj(map[string]any{
		"intent_id": idp("本任务要终止的意图 ID"),
		"reason":    str("取消或终止原因（可选）"),
	}, "intent_id"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		ri := RunInfoFrom(ctx)
		if ri.AgentKey != role || ri.IntentID != 0 || t.taskID <= 0 || ri.TaskID != t.taskID {
			return actool.Errorf(name + " 仅允许对应角色在当前任务内调用"), nil
		}
		if err := ctx.Err(); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if stop == nil {
			return actool.Errorf(name + " 当前不可用"), nil
		}
		var in struct {
			IntentID json.RawMessage `json:"intent_id"`
			Reason   string          `json:"reason"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return actool.Errorf("参数格式错误: " + err.Error()), nil
		}
		id := pid(in.IntentID)
		if id <= 0 {
			return actool.Errorf("intent_id 必须为正整数"), nil
		}
		node, err := t.ts.GetNode(id)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if node == nil || node.Kind != db.KindIntent {
			return actool.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）"), nil
		}
		// A user's stop must remain available even after asset access is revoked.
		if role == "planner" && !t.nodeAuthorized(node) {
			return actool.Errorf("该意图关联的任务资产未获授权或已被封禁"), nil
		}
		result, err := stop(ctx, t.taskID, id, strings.TrimSpace(in.Reason))
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(result)
	})
}
