package server

import (
	"context"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestTaskPendingTriggers(t *testing.T) {
	task := &Task{notify: make(chan struct{}, 1)}
	if task.hasPendingTriggers() {
		t.Fatal("new task unexpectedly has a pending trigger")
	}
	task.pendingTriggers = append(task.pendingTriggers, agent.TriggerEvent{Kind: "done", IntentID: 1})
	if !task.hasPendingTriggers() {
		t.Fatal("queued trigger was not observed")
	}
	if got := task.drainTriggers(); len(got) != 1 || task.hasPendingTriggers() {
		t.Fatalf("drainTriggers()=%v pending=%v", got, task.hasPendingTriggers())
	}
}

func TestUnchangedHeartbeatDoesNotCallPlannerWithRunningWorker(t *testing.T) {
	isolateStopTestAgentHooks(t)
	s, task := modeServer(t)
	s.engine.debounce = time.Millisecond
	if _, err := task.Store.AddNode(db.KindGoal, map[string]any{"text": "keep supervising"}, 0, "open", "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "long running worker"}, 0, "running", "test", nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("等待现有Worker，不新增工作")}}, "end_turn", llm.Usage{}, nil
	}}
	planner := agent.NewPlanner(provider, "", t.TempDir(), nil, 200000, 3)
	planner.SetNonStreaming(func() bool { return true })
	s.engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return planner, nil })
	rev, err := task.Store.BlackboardRevision()
	if err != nil {
		t.Fatal(err)
	}
	s.engine.plannerRevision.Store(task.ID, rev)
	s.engine.runPlannerRound(t.Context(), task, "heartbeat")
	if calls != 0 {
		t.Fatal("unchanged heartbeat called the model")
	}
	if _, ok := s.engine.plannerRound.Load(task.ID); ok {
		t.Fatal("unchanged heartbeat created planning round")
	}
	if _, err = task.Store.AddNode(db.KindFact, map[string]any{"summary": "new relevant evidence"}, 0, "confirmed", "test", nil); err != nil {
		t.Fatal(err)
	}
	s.engine.runPlannerRound(t.Context(), task, "heartbeat")
	if calls != 1 {
		t.Fatalf("real change model calls=%d want=1", calls)
	}
}
