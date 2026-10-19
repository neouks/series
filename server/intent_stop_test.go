package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func stopTestContext(t *testing.T, task *Task, role string) (context.Context, int64) {
	t.Helper()
	id, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: id, AgentKey: role}), id
}

func stopTestIntent(t *testing.T, task *Task, summary string) int64 {
	t.Helper()
	id, err := task.Store.AddIntent(map[string]any{"summary": summary}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func isolateStopTestAgentHooks(t *testing.T) {
	t.Helper()
	resolve, augment := agent.ToolResolve, agent.ToolAugment
	prompt, finding := agent.PromptOverride, agent.FindingTrafficBindingEnabled
	t.Cleanup(func() {
		agent.ToolResolve, agent.ToolAugment = resolve, augment
		agent.PromptOverride, agent.FindingTrafficBindingEnabled = prompt, finding
	})
	agent.ToolResolve, agent.ToolAugment = nil, nil
	agent.PromptOverride, agent.FindingTrafficBindingEnabled = nil, nil
}

func TestPlannerStopQueuedIntentAndBoundaries(t *testing.T) {
	s, task := modeServer(t)
	ctx, taskID := stopTestContext(t, task, "planner")
	id := stopTestIntent(t, task, "queued")
	activityID, err := task.Store.AppendActivity(db.Activity{Worker: "worker", NodeID: &id, Kind: "text", Summary: "keep this output", Detail: "original output"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := s.engine.KillWork(ctx, taskID, id, "duplicate")
		if err != nil || result.State != "stopped" || result.CancelledByUser {
			t.Fatalf("stop: %+v %v", result, err)
		}
	}
	node, err := task.Store.GetNode(id)
	if err != nil || node.State != "stopped" || node.UserCancelled() {
		t.Fatal("planner stop changed user lock", node, err)
	}
	if next := s.engine.claimNext(task, "test"); next != nil {
		t.Fatal("stopped intent was reclaimed", next)
	}
	if detail, err := task.Store.ActivityDetail(activityID); err != nil || detail != "original output" {
		t.Fatal("existing output lost", detail, err)
	}
	acts, _, err := task.Store.ActivityList(&id, 0, 100)
	if err != nil || len(acts) != 2 {
		t.Fatalf("audit duplicated or missing: %d %v", len(acts), err)
	}
	if events := task.drainTriggers(); len(events) != 0 {
		t.Fatal("planner stop emitted user cancellation", events)
	}
	for _, state := range []string{"paused", "done", "blocked", "exhausted"} {
		other := stopTestIntent(t, task, state)
		if err := task.Store.SetIntentState(other, state); err != nil {
			t.Fatal(err)
		}
		if _, err := s.engine.KillWork(ctx, taskID, other, "wrong state"); err == nil {
			t.Fatalf("overwrote %s", state)
		}
		if n, _ := task.Store.GetNode(other); n.State != state {
			t.Fatal("state changed", n)
		}
	}
	userID := stopTestIntent(t, task, "user stopped")
	userCtx, _ := stopTestContext(t, task, "mainagent")
	if _, err := s.cancelIntentFromMain(userCtx, taskID, userID, "user reason"); err != nil {
		t.Fatal(err)
	}
	if result, err := s.engine.KillWork(ctx, taskID, userID, "planner reason"); err != nil || !result.CancelledByUser {
		t.Fatal(result, err)
	}
	userNode, _ := task.Store.GetNode(userID)
	if !strings.Contains(string(userNode.Payload), "user reason") {
		t.Fatal("user reason replaced", string(userNode.Payload))
	}
}

func TestIntentStopHostRejectsWrongTaskRoleAndDeletion(t *testing.T) {
	s, task := modeServer(t)
	id := stopTestIntent(t, task, "untouched")
	other, err := s.m.CreateTask("foreign task", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.DeleteTask(other.ID, DeleteTaskOptions{})
	foreign := stopTestIntent(t, other, "foreign")
	for _, role := range []string{"mainagent", "planner"} {
		stop := s.cancelIntentFromMain
		if role == "planner" {
			stop = s.engine.KillWork
		}
		ctx, taskID := stopTestContext(t, task, role)
		if _, err := stop(t.Context(), taskID, id, "missing identity"); err == nil {
			t.Fatal("unscoped host call allowed")
		}
		if _, err := stop(ctx, taskID, foreign, "foreign"); err == nil {
			t.Fatal("foreign intent stopped")
		}
		bad := agent.WithRunInfo(ctx, agent.RunInfo{TaskID: taskID, AgentKey: "worker", IntentID: id})
		if _, err := stop(bad, taskID, id, "worker"); err == nil {
			t.Fatal("worker gained stop access")
		}
		s.engine.deleting.Store(task.ID, true)
		_, err := stop(ctx, taskID, id, "delete barrier")
		s.engine.deleting.Delete(task.ID)
		if err == nil {
			t.Fatal("deletion barrier bypassed")
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := stop(canceled, taskID, id, "already canceled"); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled operation allowed", err)
		}
	}
	if n, _ := task.Store.GetNode(id); n.State != "open" {
		t.Fatal("rejected calls changed intent", n)
	}
	if n, _ := other.Store.GetNode(foreign); n.State != "open" {
		t.Fatal("foreign intent changed", n)
	}
}

func TestPlannerStopWaitsForClaimRegistrationAndSettlement(t *testing.T) {
	s, task := modeServer(t)
	ctx, taskID := stopTestContext(t, task, "planner")
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	id := stopTestIntent(t, task, "claim gap")
	if ok, err := task.Store.ClaimIntent(id, "worker"); err != nil || !ok {
		t.Fatal("claim", ok, err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.engine.KillWork(ctx, taskID, id, "stop gap"); done <- err }()
	select {
	case err := <-done:
		t.Fatal("returned before registration", err)
	case <-time.After(30 * time.Millisecond):
	}
	workCtx, workCancel := context.WithCancelCause(ctx)
	defer workCancel(nil)
	s.engine.registerWork(id, workCancel)
	<-workCtx.Done()
	if code, _, _, _ := agent.AbortReason(workCtx); code != "killed_by_planner" {
		t.Fatal("wrong cause", code)
	}
	action, complete := s.engine.detachWork(id)
	if action != "kill" {
		t.Fatal("wrong action", action)
	}
	want := errors.New("failed to persist stopped state")
	complete(want)
	if err := <-done; !errors.Is(err, want) {
		t.Fatal("settlement failure reported as success", err)
	}
}

func TestPlannerStopTimeoutDoesNotReportSuccess(t *testing.T) {
	s, task := modeServer(t)
	ctx, taskID := stopTestContext(t, task, "planner")
	id := stopTestIntent(t, task, "slow settlement")
	if ok, err := task.Store.ClaimIntent(id, "worker"); err != nil || !ok {
		t.Fatal("claim", ok, err)
	}
	workCtx, workCancel := context.WithCancelCause(t.Context())
	defer workCancel(nil)
	s.engine.registerWork(id, workCancel)
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	result, err := s.engine.KillWork(waitCtx, taskID, id, "slow")
	if !errors.Is(err, context.DeadlineExceeded) || result.State == "stopped" {
		t.Fatalf("timeout reported success: %+v %v", result, err)
	}
	if code, _, _, _ := agent.AbortReason(workCtx); code != "killed_by_planner" {
		t.Fatal("cancellation signal lost", code)
	}
	action, complete := s.engine.detachWork(id)
	complete(nil)
	if action != "" {
		t.Fatal("timeout retained controller reservation", action)
	}
	if n, _ := task.Store.GetNode(id); n.State != "running" {
		t.Fatal("state changed before worker exited", n)
	}
}

func TestIntentStopRunningWorkerSettlement(t *testing.T) {
	isolateStopTestAgentHooks(t)
	for _, role := range []string{"mainagent", "planner"} {
		t.Run(role, func(t *testing.T) {
			s, task := modeServer(t)
			ctx, taskID := stopTestContext(t, task, role)
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			id := stopTestIntent(t, task, "running")
			if ok, err := task.Store.ClaimIntent(id, "worker"); err != nil || !ok {
				t.Fatal("claim", ok, err)
			}
			intent, _ := task.Store.GetNode(id)
			started := make(chan struct{})
			cause := make(chan string, 1)
			lateFact := make(chan int64, 1)
			provider := retestProvider{complete: func(call context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				close(started)
				<-call.Done()
				code, _, _, _ := agent.AbortReason(call)
				cause <- code
				// Model output that is already being persisted when stop arrives must
				// finish before the caller is told cancellation has settled.
				fact, err := task.Store.AddNode(db.KindFact, map[string]any{"summary": "last fact"}, 1, "confirmed", "worker", nil)
				if err != nil {
					return llm.Message{}, "", llm.Usage{}, err
				}
				lateFact <- fact
				return llm.Message{}, "", llm.Usage{}, call.Err()
			}}
			worker := agent.NewWorker(provider, "test", t.TempDir(), nil, 100000, 10)
			worker.SetNonStreaming(func() bool { return true })
			done := make(chan bool, 1)
			go func() { done <- s.engine.runIntent(ctx, task, "worker", worker, intent, "", "") }()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("worker did not start", ctx.Err())
			}
			stop := s.cancelIntentFromMain
			wantCause := "work_cancelled_by_user"
			if role == "planner" {
				stop, wantCause = s.engine.KillWork, "killed_by_planner"
			}
			result, err := stop(ctx, taskID, id, "test reason")
			if err != nil || result.State != "stopped" || result.CancelledByUser != (role == "mainagent") {
				t.Fatalf("stop: %+v %v", result, err)
			}
			if got := <-cause; got != wantCause {
				t.Fatalf("cause=%q want=%q", got, wantCause)
			}
			if !<-done {
				t.Fatal("worker failed to settle")
			}
			select {
			case fact := <-lateFact:
				if n, err := task.Store.GetNode(fact); err != nil || n == nil {
					t.Fatal("last output lost", n, err)
				}
			default:
				t.Fatal("stop succeeded before final output was persisted")
			}
			node, err := task.Store.GetNode(id)
			if err != nil || node.State != "stopped" || node.UserCancelled() != (role == "mainagent") {
				t.Fatal("wrong final state", node, err)
			}
			if ctx.Err() != nil || s.engine.execContextFor(ctx, task.ID).Err() != nil {
				t.Fatal("stopping one worker canceled task")
			}
		})
	}
}

func TestMainChatCancelsIntentThroughCatalog(t *testing.T) {
	isolateStopTestAgentHooks(t)
	s, task := modeServer(t)
	_, taskID := stopTestContext(t, task, "mainagent")
	id := stopTestIntent(t, task, "cancel via chat")
	wireTools(s.m.pg, buildDomainReg(s.m.assets))
	calls := 0
	provider := retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			hasCancel := false
			for _, tool := range req.Tools {
				hasCancel = hasCancel || tool.Name == "cancel_intent"
				if tool.Name == "kill_work" {
					t.Error("main agent received planner termination")
				}
			}
			if !hasCancel {
				t.Error("catalog omitted cancel_intent")
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "cancel-test", Name: "cancel_intent", Input: json.RawMessage(fmt.Sprintf(`{"intent_id":%d,"reason":"用户要求停止"}`, id))}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("已取消")}}, "end_turn", llm.Usage{}, nil
	}}
	main := agent.NewMainAgent(provider, "test", t.TempDir(), nil, 100000, 4)
	main.SetNonStreaming(func() bool { return true })
	main.SetCancelIntent(s.cancelIntentFromMain)
	_, err := main.Chat(t.Context(), taskID, 1, nil, nil, task.Store, "goal", fmt.Sprintf("取消意图 #%d", id), nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	node, err := task.Store.GetNode(id)
	if err != nil || node.State != "stopped" || !node.UserCancelled() || !strings.Contains(string(node.Payload), "用户要求停止") {
		t.Fatal("chat failed to cancel", node, err)
	}
	if events := task.drainTriggers(); len(events) != 1 || events[0].Kind != "cancelled" {
		t.Fatal("Planner not notified", events)
	}
}

func TestPlannerStopThroughPlan(t *testing.T) {
	isolateStopTestAgentHooks(t)
	s, task := modeServer(t)
	_, taskID := stopTestContext(t, task, "planner")
	id := stopTestIntent(t, task, "queued planner tool")
	wireTools(s.m.pg, buildDomainReg(s.m.assets))
	calls := 0
	provider := retestProvider{complete: func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls == 1 {
			for _, tool := range req.Tools {
				if tool.Name == "cancel_intent" {
					t.Error("Planner received user cancellation tool")
				}
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "kill-test", Name: "kill_work", Input: json.RawMessage(fmt.Sprintf(`{"intent_id":%d,"reason":"重复方向"}`, id))}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("重复方向已终止")}}, "end_turn", llm.Usage{}, nil
	}}
	planner := agent.NewPlanner(provider, "test", t.TempDir(), nil, 100000, 4)
	planner.SetNonStreaming(func() bool { return true })
	planner.SetKillWork(s.engine.KillWork)
	if _, _, err := planner.Plan(t.Context(), taskID, nil, nil, task.Store, "goal", nil, nil); err != nil {
		t.Fatal(err)
	}
	node, err := task.Store.GetNode(id)
	if err != nil || node.State != "stopped" || node.UserCancelled() {
		t.Fatal("Planner tool failed to stop queued intent", node, err)
	}
}
