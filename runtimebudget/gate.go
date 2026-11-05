// Package runtimebudget bounds host resources without changing authorization.
package runtimebudget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

var (
	ErrQueueFull = errors.New("资源等待队列已满，请稍后重试")
	ErrReleased  = errors.New("资源预留已取消")
	ErrClosed    = errors.New("资源准入已关闭")
)

// LocalError must never rotate a model profile, trip its circuit breaker or
// replay a whole Worker: no remote execution was attempted.
type LocalError struct{ Cause error }

func (e *LocalError) Error() string             { return e.Cause.Error() }
func (e *LocalError) Unwrap() error             { return e.Cause }
func (*LocalError) LocalAdmissionFailure() bool { return true }

func IsLocalError(err error) bool {
	var local interface{ LocalAdmissionFailure() bool }
	return errors.As(err, &local) && local.LocalAdmissionFailure()
}

// StopsAutomaticReplay includes admission failures and request-policy stops
// after remote work may already have started. Neither is a quota diagnosis.
func StopsAutomaticReplay(err error) bool {
	if IsLocalError(err) || IsContextCapacityRejection(err) {
		return true
	}
	var stop interface{ PreventModelReplay() bool }
	return errors.As(err, &stop) && stop.PreventModelReplay()
}

type Limits struct {
	Concurrency int `json:"concurrency"`
	Queue       int `json:"queue"`
}

func (l *Limits) UnmarshalJSON(raw []byte) error {
	type plain Limits
	var next plain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("资源配置必须是一个JSON对象")
	}
	if err := Limits(next).Validate(); err != nil {
		return err
	}
	*l = Limits(next)
	return nil
}

func (l Limits) Validate() error {
	if l.Concurrency < 1 || l.Concurrency > 128 || l.Queue < 1 || l.Queue > 4096 {
		return fmt.Errorf("资源并发数须为1..128，等待队列须为1..4096")
	}
	return nil
}

type Snapshot struct {
	Limits
	Active int `json:"active"`
	Queued int `json:"queued"`
}

// Gate gives each waiting task one turn per cycle, FIFO within a task. Memory
// is bounded by active+queued reservations, not the number of historical tasks.
type Gate struct {
	mu         sync.Mutex
	limits     Limits
	active     int
	activeKeys map[string]int
	queued     int
	closed     bool
	order      []string
	queues     map[string][]*Permit
}

type permitState uint8

const (
	waiting permitState = iota
	granted
	released
)

type Permit struct {
	gate            *Gate
	key             string
	ready           chan struct{}
	state           permitState // protected by gate.mu
	queuedInitially bool        // immutable after ReserveWithin returns
	err             error
	keyLimit        int // zero: no per-key cap; otherwise shared by all entry paths
}

func New(limits Limits) (*Gate, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &Gate{limits: limits, queues: map[string][]*Permit{}, activeKeys: map[string]int{}}, nil
}

// Reserve is nonblocking, so a caller can reject overload before changing a
// persistent execution state. Release the permit on every failed setup path.
func (g *Gate) Reserve(key string) (*Permit, error) {
	return g.ReserveWithin(key, 0)
}

// ReserveWithin also limits simultaneous execution for this key. Callers must
// use the same trusted lifetime policy for every entry path of a key. Saturated
// keys never block an eligible task behind them. Zero retains Reserve behavior.
func (g *Gate) ReserveWithin(key string, keyLimit int) (*Permit, error) {
	if keyLimit < 0 {
		return nil, fmt.Errorf("per-key concurrency must be nonnegative")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, &LocalError{ErrClosed}
	}
	g.dispatch()
	p := &Permit{gate: g, key: key, keyLimit: keyLimit, ready: make(chan struct{})}
	if g.active < g.limits.Concurrency && (keyLimit == 0 || g.activeKeys[key] < keyLimit) && len(g.queues[key]) == 0 {
		p.state = granted
		g.active++
		g.activeKeys[key]++
		close(p.ready)
		return p, nil
	}
	if g.queued >= g.limits.Queue {
		return nil, &LocalError{ErrQueueFull}
	}
	if len(g.queues[key]) == 0 {
		g.order = append(g.order, key)
	}
	g.queues[key] = append(g.queues[key], p)
	p.queuedInitially = true
	g.queued++
	return p, nil
}

// WasQueued lets callers refresh configuration after a real wait without
// duplicating configuration reads on the uncontended admission path.
func (p *Permit) WasQueued() bool { return p.queuedInitially }

func (p *Permit) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		p.Release()
		return ctx.Err()
	case <-p.ready:
	}
	// Cancellation can race an immediate grant. Never start remote work with
	// a canceled context merely because both select cases were ready.
	if err := ctx.Err(); err != nil {
		p.Release()
		return err
	}
	p.gate.mu.Lock()
	if p.gate.closed {
		p.gate.mu.Unlock()
		p.Release()
		return &LocalError{ErrClosed}
	}
	defer p.gate.mu.Unlock()
	if p.state != granted {
		if p.err != nil {
			return p.err
		}
		return &LocalError{ErrReleased}
	}
	return nil
}

func (g *Gate) Acquire(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := g.Reserve(key)
	if err != nil {
		return nil, err
	}
	if err = p.Wait(ctx); err != nil {
		p.Release()
		return nil, err
	}
	return p.Release, nil
}

func (p *Permit) Release() {
	g := p.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	switch p.state {
	case released:
		return
	case granted:
		g.active--
		g.activeKeys[p.key]--
		if g.activeKeys[p.key] == 0 {
			delete(g.activeKeys, p.key)
		}
	case waiting:
		q := g.queues[p.key]
		for i, item := range q {
			if item == p {
				copy(q[i:], q[i+1:])
				q[len(q)-1] = nil
				g.queues[p.key] = q[:len(q)-1]
				g.queued--
				break
			}
		}
		if len(g.queues[p.key]) == 0 {
			g.removeKey(p.key)
		}
		close(p.ready)
	}
	p.state = released
	g.dispatch()
}

func (g *Gate) removeKey(key string) {
	delete(g.queues, key)
	for i, k := range g.order {
		if k == key {
			copy(g.order[i:], g.order[i+1:])
			g.order[len(g.order)-1] = ""
			g.order = g.order[:len(g.order)-1]
			return
		}
	}
}

func (g *Gate) dispatch() {
	for !g.closed && g.active < g.limits.Concurrency && len(g.order) > 0 {
		eligible := -1
		for i, key := range g.order {
			p := g.queues[key][0]
			if p.keyLimit == 0 || g.activeKeys[key] < p.keyLimit {
				eligible = i
				break
			}
		}
		if eligible < 0 {
			return
		}
		key := g.order[eligible]
		q := g.queues[key]
		p := q[0]
		q[0] = nil
		q = q[1:]
		copy(g.order[eligible:], g.order[eligible+1:])
		g.order[len(g.order)-1] = ""
		g.order = g.order[:len(g.order)-1]
		if len(q) == 0 {
			delete(g.queues, key)
		} else {
			g.queues[key] = q
			g.order = append(g.order, key)
		}
		g.queued--
		g.active++
		g.activeKeys[key]++
		p.state = granted
		close(p.ready)
	}
}

// Lowering the cap drains existing work; it never cancels a running command.
// Existing waiters survive a smaller queue cap, but no extra waiters are added.
func (g *Gate) Configure(limits Limits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return &LocalError{ErrClosed}
	}
	g.limits = limits
	g.dispatch()
	return nil
}

func (g *Gate) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Snapshot{Limits: g.limits, Active: g.active, Queued: g.queued}
}

func (g *Gate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	g.closed = true
	for _, q := range g.queues {
		for _, p := range q {
			p.state, p.err = released, &LocalError{ErrClosed}
			close(p.ready)
		}
	}
	g.queues, g.order, g.queued = map[string][]*Permit{}, nil, 0
}

// All Config-built providers share this process boundary, across profiles and
// roles. Profile rate limits remain independent and continue to apply inside.
var modelGate, _ = New(Limits{Concurrency: 8, Queue: 256})

func ModelGate() *Gate { return modelGate }

// Worker executions occupy a slot through final state settlement, independently
// of model-request slots. Manual resumes and pool claims share this boundary.
var workerGate, _ = New(Limits{Concurrency: 16, Queue: 256})

func WorkerGate() *Gate { return workerGate }
