package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/neouks/series/runtimebudget"
)

type modelDeadlineKey struct{}

type modelCallDeadline struct {
	ctx           context.Context
	cancel        context.CancelCauseFunc
	cleanup       func()
	headers, idle time.Duration
	started       time.Time
	reported      sync.Once
	bytes         modelByteBudget
}

func newModelCallDeadline(ctx context.Context, header, idle, total time.Duration, bytePolicy ...runtimebudget.ModelByteLimits) (*modelCallDeadline, context.Context) {
	totalCtx, totalCancel := context.WithTimeoutCause(ctx, total, &runtimebudget.RequestTimeout{Phase: "total", Limit: total})
	callCtx, cancel := context.WithCancelCause(totalCtx)
	var once sync.Once
	owner := &modelCallDeadline{ctx: callCtx, cancel: cancel, headers: header, idle: idle, started: time.Now()}
	owner.bytes.limits = runtimebudget.ModelBytePolicy(ctx)
	if len(bytePolicy) > 0 {
		owner.bytes.limits = bytePolicy[0]
	}
	callCtx = runtimebudget.WithModelByteLimits(callCtx, owner.bytes.limits)
	owner.ctx = callCtx
	owner.cleanup = func() { once.Do(func() { cancel(context.Canceled); totalCancel() }) }
	return owner, context.WithValue(callCtx, modelDeadlineKey{}, owner)
}

func (d *modelCallDeadline) stopError(err error) error {
	cause := context.Cause(d.ctx)
	if cause == nil {
		return err
	}
	var stopped *runtimebudget.RequestTimeout
	if errors.As(cause, &stopped) {
		d.reported.Do(func() {
			ri := RunInfoFrom(d.ctx)
			log.Printf("[model-policy-timeout] task=%d role=%s phase=%s limit_ms=%d elapsed_ms=%d", ri.TaskID, ri.AgentKey, stopped.Phase, stopped.Limit.Milliseconds(), time.Since(d.started).Milliseconds())
		})
		return stopped
	}
	var bytesStopped *runtimebudget.ModelByteLimitError
	if errors.As(cause, &bytesStopped) {
		d.reported.Do(func() {
			ri := RunInfoFrom(d.ctx)
			log.Printf("[model-byte-limit] task=%d role=%s phase=%s limit_bytes=%d", ri.TaskID, ri.AgentKey, bytesStopped.Phase, bytesStopped.Limit)
		})
		return bytesStopped
	}
	return err
}

func modelRequestError(ctx context.Context, err error) error {
	if owner, _ := ctx.Value(modelDeadlineKey{}).(*modelCallDeadline); owner != nil {
		return owner.stopError(err)
	}
	return err
}

// A phase timer is armed only while RoundTrip or Read is actually blocked.
// Downstream backpressure between reads is covered by total time, not mistaken
// for network inactivity. The shared logical context stops official SDK retries.
func (d *modelCallDeadline) watch(phase string, limit time.Duration) func() {
	var mu sync.Mutex
	done := false
	timer := time.AfterFunc(limit, func() {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			done = true
			d.cancel(&runtimebudget.RequestTimeout{Phase: phase, Limit: limit})
		}
	})
	return func() { mu.Lock(); done = true; timer.Stop(); mu.Unlock() }
}

type timedModelTransport struct{ base http.RoundTripper }

func (t timedModelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	owner, _ := req.Context().Value(modelDeadlineKey{}).(*modelCallDeadline)
	cleanup := func() {}
	if owner == nil {
		// Direct connection checks also have a finite lifetime. Config providers
		// normally arrive with the owner installed before permission refresh.
		limits := runtimebudget.ModelDeadlineSnapshot()
		var ctx context.Context
		owner, ctx = newModelCallDeadline(req.Context(), time.Duration(limits.HeaderSeconds)*time.Second, time.Duration(limits.ReadIdleSeconds)*time.Second, time.Duration(limits.TotalSeconds)*time.Second)
		req = req.Clone(ctx)
		cleanup = owner.cleanup
	}
	// Closing one response must unblock its Read without canceling the entire
	// logical call (a normal SDK retry may follow a non-200 response).
	attempt, cancelAttempt := context.WithCancel(req.Context())
	req = req.Clone(attempt)
	finishOwner := cleanup
	cleanup = func() { cancelAttempt(); finishOwner() }
	stop := owner.watch("headers", owner.headers)
	response, err := t.base.RoundTrip(req)
	stop()
	if err != nil || response == nil {
		err = owner.stopError(err)
		cleanup()
		return response, err
	}
	response.Body = &timedModelBody{ReadCloser: response.Body, owner: owner, cleanup: cleanup}
	return response, nil
}

type timedModelBody struct {
	io.ReadCloser
	owner     *modelCallDeadline
	cleanup   func()
	once      sync.Once
	watchOnce sync.Once
	readWatch modelReadWatch
}

// Reuse one timer for an entire response. Creating an AfterFunc for every SSE
// chunk caused five allocations per Read. Old callbacks recheck the new deadline
// under the mutex, so Stop/Reset races cannot cancel a later healthy read.
type modelReadWatch struct {
	mu            sync.Mutex
	owner         *modelCallDeadline
	timer         *time.Timer
	deadline      time.Time
	armed, closed bool
}

func (w *modelReadWatch) start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.armed = true
	w.deadline = time.Now().Add(w.owner.idle)
	if w.timer == nil {
		w.timer = time.AfterFunc(w.owner.idle, w.fire)
	} else {
		w.timer.Reset(w.owner.idle)
	}
}

func (w *modelReadWatch) fire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || !w.armed {
		return
	}
	if left := time.Until(w.deadline); left > 0 {
		w.timer.Reset(left)
		return
	}
	w.armed = false
	w.owner.cancel(&runtimebudget.RequestTimeout{Phase: "read_idle", Limit: w.owner.idle})
}

func (w *modelReadWatch) pause(close bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = false
	w.closed = w.closed || close
	if w.timer != nil {
		w.timer.Stop()
	}
}

func (b *timedModelBody) Read(p []byte) (int, error) {
	b.watchOnce.Do(func() { b.readWatch.owner = b.owner })
	b.readWatch.start()
	n, err := b.owner.readResponse(b.ReadCloser, p)
	b.readWatch.pause(false)
	err = b.owner.stopError(err)
	if err != nil {
		b.readWatch.pause(true)
		b.once.Do(b.cleanup)
	}
	return n, err
}

func (b *timedModelBody) Close() error {
	b.readWatch.pause(true)
	b.once.Do(b.cleanup)
	return b.ReadCloser.Close()
}
