package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func (e *Engine) intentControlTask(ctx context.Context, taskID int64, role string) (*Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ri := agent.RunInfoFrom(ctx)
	if taskID <= 0 || ri.TaskID != taskID || ri.AgentKey != role || ri.IntentID != 0 || e.m == nil {
		return nil, fmt.Errorf("意图控制角色或任务上下文不匹配")
	}
	t, ok := e.m.Task(strconv.FormatInt(taskID, 10))
	if !ok {
		return nil, fmt.Errorf("task not found")
	}
	return t, nil
}

// cancelIntentFromMain deliberately shares the UI's cancellation lock, reason,
// notification and settlement behavior. A catalog binding alone grants no access.
func (s *Server) cancelIntentFromMain(ctx context.Context, taskID, intentID int64, reason string) (agent.IntentStopResult, error) {
	t, err := s.engine.intentControlTask(ctx, taskID, "mainagent")
	if err != nil {
		return agent.IntentStopResult{}, err
	}
	if !s.engine.beginTaskOperation(t.ID) {
		return agent.IntentStopResult{}, fmt.Errorf("任务正在删除，无法取消意图")
	}
	defer s.engine.decInflight(t.ID)
	result, err := s.applyIntentControl(ctx, t, intentID, "cancel", reason)
	if err != nil {
		return agent.IntentStopResult{}, err
	}
	return agent.IntentStopResult{ID: result.ID, State: result.State, Reason: result.CancelReason, CancelledByUser: result.CancelledByUser}, nil
}

// KillWork stops a local queued or running intent. Admission is serialized with
// claims and user controls; a claimed row may exist briefly before registerWork.
// Running work must finish its writes before this callback reports success.
func (e *Engine) KillWork(ctx context.Context, taskID, intentID int64, reason string) (agent.IntentStopResult, error) {
	t, err := e.intentControlTask(ctx, taskID, "planner")
	if err != nil {
		return agent.IntentStopResult{}, err
	}
	if !e.beginTaskOperation(t.ID) {
		return agent.IntentStopResult{}, fmt.Errorf("任务正在删除，无法终止意图")
	}
	defer e.decInflight(t.ID)
	t.workerControlMu.Lock()
	defer t.workerControlMu.Unlock()
	waitCtx, cancel := context.WithTimeout(ctx, workControlWaitTimeout)
	defer cancel()
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "规划者判定该方向无需继续执行"
	}
	out := agent.IntentStopResult{ID: intentID, State: "stopped", Reason: reason}
	changed := false
	for {
		if err := waitCtx.Err(); err != nil {
			return agent.IntentStopResult{}, err
		}
		node, err := t.Store.GetNode(intentID)
		if err != nil {
			return agent.IntentStopResult{}, err
		}
		if node == nil || node.Kind != db.KindIntent {
			return agent.IntentStopResult{}, fmt.Errorf("intent_id 必须是本任务的意图（关联任务意图只读）")
		}
		if node.UserCancelled() {
			// Preserve the user's execution lock and original cancellation reason.
			out.CancelledByUser, out.Reason = true, "意图已由用户取消"
			return out, nil
		}
		switch node.State {
		case "stopped":
			if changed {
				e.emitActivity(t, db.Activity{Worker: "planner", NodeID: &intentID, Kind: "text",
					Summary: fmt.Sprintf("规划者终止意图 #%d", intentID),
					Detail:  "中断原因：killed_by_planner；终止原因：" + reason + "。意图及已有产出保留。"})
				e.touch(t.ID)
			} else {
				out.Reason = "意图已经停止，本次未重复终止"
			}
			return out, nil
		case "open":
			ok, err := t.Store.CompareAndSetIntentState(intentID, "open", "stopped")
			if err != nil {
				return agent.IntentStopResult{}, err
			}
			changed = changed || ok
		case "running":
			if err := e.controlWork(waitCtx, intentID, "kill", agent.AbortKilledByPlanner); err != nil {
				if !errors.Is(err, errWorkControlConflict) {
					return agent.IntentStopResult{}, err
				}
				// Registration/settlement may be in progress. Re-read state before retry.
				if sleepCtx(waitCtx, 10*time.Millisecond) {
					return agent.IntentStopResult{}, waitCtx.Err()
				}
			} else {
				changed = true
			}
		default:
			return agent.IntentStopResult{}, fmt.Errorf("仅等待中或运行中的意图可以由 Planner 终止（当前状态 %s）", node.State)
		}
	}
}
