package server

import (
	"context"
	"testing"
	"time"

	"github.com/neouks/series/db"
)

func pauseClockServer(t *testing.T) (*Server, *Task) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := m.CreateTask("pause clock", "goal", nil, 3600, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.DeleteTask(task.ID, DeleteTaskOptions{}); m.Close() })
	return newAdmissionTestServer(m, nil), task
}

// Pausing freezes the task-level timeout budget: paused_at is recorded on
// pause, and the resume admission shifts deadline_at forward by the paused
// interval so the countdown continues where it stopped.
func TestResumeShiftsDeadlineByPausedInterval(t *testing.T) {
	s, task := pauseClockServer(t)
	dl, err := s.m.StampTaskFirstRun(task.ID)
	if err != nil || dl <= 0 {
		t.Fatalf("StampTaskFirstRun=(%d,%v)", dl, err)
	}
	if _, err := s.applyTaskControl(task, "pause"); err != nil {
		t.Fatal(err)
	}
	var pausedAt *time.Time
	if err := s.m.pg.QueryRow(`SELECT paused_at FROM tasks WHERE id=$1`, task.ID).Scan(&pausedAt); err != nil {
		t.Fatal(err)
	}
	if pausedAt == nil {
		t.Fatal("暂停未记录 paused_at")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := s.applyTaskControl(task, "resume"); err != nil {
		t.Fatal(err)
	}
	state := task.lifecycleSnapshot()
	if state.DeadlineAt < dl+1 {
		t.Fatalf("恢复未按暂停时长顺延截止：deadline %d → %d（暂停约 1.1s）", dl, state.DeadlineAt)
	}
	if err := s.m.pg.QueryRow(`SELECT paused_at FROM tasks WHERE id=$1`, task.ID).Scan(&pausedAt); err != nil {
		t.Fatal(err)
	}
	if pausedAt != nil {
		t.Fatal("恢复后 paused_at 未清空")
	}
}

// The deadline coordinator must hold an expired task while it is paused
// instead of driving the settle sequence.
func TestDeadlineCoordinatorHoldsPausedTask(t *testing.T) {
	s, task := pauseClockServer(t)
	if _, err := s.m.StampTaskFirstRun(task.ID); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-5 * time.Second).Unix()
	task.updateLifecycle(func(st *taskLifecycleState) { st.DeadlineAt = past })
	s.engine.deadline.Store(task.ID, past)
	if _, err := s.applyTaskControl(task, "pause"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	s.engine.deadlineCoordinator(ctx, task)
	if s.engine.isSettling(task.ID) {
		t.Fatal("暂停中的任务被超时收尾")
	}
	if status := s.m.TaskStatus(task.ID); status == "timeout" {
		t.Fatal("暂停中的任务被置为超时")
	}
}

// Companion guard: with the pause lifted, an expired deadline still settles.
// Manual execution mode keeps the terminal planner out of the fixture (same as
// TestManualModeSkipsTerminalPlannerOnTimeout) so the settle runs to its
// terminal status without a live LLM.
func TestDeadlineCoordinatorSettlesExpiredUnpausedTask(t *testing.T) {
	s, task := pauseClockServer(t)
	if _, err := task.Store.SetExecutionMode(db.ExecutionManual); err != nil {
		t.Fatal(err)
	}
	task.updateLifecycle(func(st *taskLifecycleState) { st.ExecutionMode = db.ExecutionManual })
	if _, err := s.m.StampTaskFirstRun(task.ID); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-5 * time.Second).Unix()
	task.updateLifecycle(func(st *taskLifecycleState) { st.DeadlineAt = past })
	s.engine.deadline.Store(task.ID, past)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.engine.deadlineCoordinator(ctx, task)
	if status := s.m.TaskStatus(task.ID); status != "timeout" {
		t.Fatalf("未暂停的过期任务未收尾：status=%q", status)
	}
}
