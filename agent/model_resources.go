package agent

import (
	"context"
	"fmt"
	"iter"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/neouks/series/runtimebudget"
)

type modelResourceProvider struct {
	llm.Provider
	gate *runtimebudget.Gate
	view bool
}

// Scope the complete logical call before recording, model selection and request
// preparation. The final Config provider consumes the same private permit.
func WithModelRequestScope(provider llm.Provider) llm.Provider {
	if provider == nil {
		return nil
	}
	if p, ok := provider.(modelResourceProvider); ok && p.view {
		return provider
	}
	return modelResourceProvider{Provider: provider, gate: runtimebudget.ModelGate(), view: true}
}

type modelLeaseKey struct{}
type modelLeaseScope struct {
	task, exploration, intent int64
	role, session             string
}
type modelLease struct {
	gate     *runtimebudget.Gate
	scope    modelLeaseScope
	mu       sync.Mutex
	active   bool
	consumed bool
	release  func()
}

func (l *modelLease) reuse(consume bool) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active || (consume && l.consumed) {
		return nil, false
	}
	if !consume {
		return func() {}, true
	}
	l.consumed = true
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.consumed = false
			done := !l.active
			l.mu.Unlock()
			if done {
				l.release()
			}
		})
	}, true
}

func (l *modelLease) finish() {
	l.mu.Lock()
	l.active = false
	done := !l.consumed
	l.mu.Unlock()
	if done {
		l.release()
	}
}

// Task model views acquire before refreshing authorization/constraints. The
// final Config provider reuses that private lease for this same logical call,
// so a queued request cannot send the stale pre-wait model view. The marker is
// valid only during the owning call and cannot grant another task/run a slot.
func acquireModelRequest(ctx context.Context, gate *runtimebudget.Gate, consume bool) (context.Context, func(), error) {
	ri := RunInfoFrom(ctx)
	scope := modelLeaseScope{ri.TaskID, ri.ExplorationID, ri.IntentID, ri.AgentKey, transcript.SessionIDFrom(ctx)}
	if lease, ok := ctx.Value(modelLeaseKey{}).(*modelLease); ok && lease.gate == gate && lease.scope == scope {
		if release, ok := lease.reuse(consume); ok {
			return ctx, release, nil
		}
	}
	limits := runtimebudget.ModelDeadlineSnapshot()
	bytePolicy := runtimebudget.ModelBytePolicy(ctx)
	if previous, ok := ctx.Value(modelLeaseKey{}).(*modelLease); ok {
		previous.mu.Lock()
		stale := !previous.active || previous.scope != scope
		previous.mu.Unlock()
		if stale {
			bytePolicy = runtimebudget.ModelByteSnapshot()
		}
	}
	release, err := (modelResourceProvider{gate: gate}).acquire(ctx, time.Duration(limits.QueueSeconds)*time.Second)
	if err != nil {
		return ctx, nil, err
	}
	deadline, ctx := newModelCallDeadline(ctx, time.Duration(limits.HeaderSeconds)*time.Second, time.Duration(limits.ReadIdleSeconds)*time.Second, time.Duration(limits.TotalSeconds)*time.Second, bytePolicy)
	var once sync.Once
	cleanup := func() { once.Do(func() { deadline.cleanup(); release() }) }
	lease := &modelLease{gate: gate, scope: scope, active: true, consumed: consume, release: cleanup}
	ctx = context.WithValue(ctx, modelLeaseKey{}, lease)
	if consume {
		return ctx, func() {
			lease.mu.Lock()
			lease.active = false
			lease.consumed = false
			lease.mu.Unlock()
			lease.release()
		}, nil
	}
	return ctx, lease.finish, nil
}

func modelResourceKey(ctx context.Context) string {
	if ri := RunInfoFrom(ctx); ri.TaskID > 0 {
		return fmt.Sprintf("task:%d", ri.TaskID)
	}
	if session := transcript.SessionIDFrom(ctx); session != "" {
		return "session:" + session
	}
	return "unscoped"
}

func (p modelResourceProvider) acquire(ctx context.Context, queue time.Duration) (func(), error) {
	started := time.Now()
	waitCtx, cancel := context.WithTimeoutCause(ctx, queue, &runtimebudget.RequestTimeout{Phase: "queue", Limit: queue})
	defer cancel()
	release, err := p.gate.Acquire(waitCtx, modelResourceKey(ctx))
	if err != nil {
		if cause, ok := context.Cause(waitCtx).(*runtimebudget.RequestTimeout); ok {
			err = cause
			ri := RunInfoFrom(ctx)
			log.Printf("[model-policy-timeout] task=%d role=%s phase=%s limit_ms=%d elapsed_ms=%d", ri.TaskID, ri.AgentKey, cause.Phase, cause.Limit.Milliseconds(), time.Since(started).Milliseconds())
		}
	}
	if err != nil || time.Since(started) >= 100*time.Millisecond {
		ri := RunInfoFrom(ctx)
		log.Printf("[model-admission] task=%d role=%s wait_ms=%d rejected=%t", ri.TaskID, ri.AgentKey, time.Since(started).Milliseconds(), err != nil)
	}
	return release, err
}

func (p modelResourceProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	ctx, release, err := acquireModelRequest(ctx, p.gate, !p.view)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	defer release()
	message, stop, usage, err := p.Provider.Complete(ctx, req)
	return message, stop, usage, modelRequestError(ctx, err)
}

func (p modelResourceProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		ctx, release, err := acquireModelRequest(ctx, p.gate, !p.view)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		defer release()
		failed := false
		for event, err := range p.Provider.Stream(ctx, req) {
			err = modelRequestError(ctx, err)
			failed = failed || err != nil
			if !yield(event, err) {
				return
			}
			if err != nil {
				return
			}
		}
		if err := modelRequestError(ctx, nil); err != nil && !failed {
			yield(llm.StreamEvent{}, err)
		}
	}
}
