package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/neouks/series/runtimebudget"
)

const settingWorkerExecutionLimits = "worker_execution_limits"
const workerAdmissionWait = 30 * time.Second

func (t *Task) workerConcurrency(m *Manager) int {
	t.workerSlotsOnce.Do(func() { t.workerSlots = m.Workers() })
	return t.workerSlots
}

func (e *Engine) workerResourceGate() *runtimebudget.Gate {
	if e.workerGate != nil {
		return e.workerGate
	}
	return runtimebudget.WorkerGate()
}

func (e *Engine) workerWaitContext(ctx context.Context, t *Task) (context.Context, context.CancelFunc) {
	ctx = e.execContextFor(ctx, t.ID)
	if deadline := e.taskDeadline(t); deadline > 0 {
		return context.WithDeadline(ctx, time.Unix(deadline, 0))
	}
	return context.WithCancel(ctx)
}

// Waiting changes no persistent intent state and holds no queue/control mutex.
// The task execution context cancels it on pause, deletion and shutdown. Manual
// callers additionally supply an HTTP-owned, finite wait context; after grant,
// only the task/server context owns execution.
func (e *Engine) acquireWorker(ctx context.Context, t *Task) (*runtimebudget.Permit, error) {
	started := time.Now()
	p, err := e.workerResourceGate().ReserveWithin(t.ID, t.workerConcurrency(e.m))
	if err == nil {
		err = p.Wait(ctx)
		if err != nil {
			p.Release()
		}
	}
	if err != nil || time.Since(started) >= 100*time.Millisecond {
		log.Printf("[worker-admission] task=%s wait_ms=%d rejected=%t", t.ID, time.Since(started).Milliseconds(), err != nil)
	}
	return p, err
}

func (m *Manager) loadWorkerExecutionLimits() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, found, err := m.pg.GetSettingContext(ctx, settingWorkerExecutionLimits)
	if err != nil {
		return err
	}
	limits := runtimebudget.Limits{Concurrency: 16, Queue: 256}
	if found {
		if err = json.Unmarshal([]byte(raw), &limits); err != nil {
			return fmt.Errorf("invalid worker_execution_limits: %w", err)
		}
	}
	return runtimebudget.WorkerGate().Configure(limits)
}

func (m *Manager) setWorkerExecutionLimits(ctx context.Context, limits runtimebudget.Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m.workerLimitsMu.Lock()
	defer m.workerLimitsMu.Unlock()
	raw, _ := json.Marshal(limits)
	if err := m.pg.SetSettingContext(ctx, settingWorkerExecutionLimits, string(raw)); err != nil {
		return err
	}
	return runtimebudget.WorkerGate().Configure(limits)
}
