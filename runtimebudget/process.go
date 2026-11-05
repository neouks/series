package runtimebudget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
	"unicode/utf8"
)

// ProcessGate is shared by project-owned external execution entry points.
// A permit counts one launched job, not every process it may fork. It must be
// held through Wait/transport Close, never merely through a tool's return.
var processGate, _ = New(Limits{Concurrency: 32, Queue: 256})

func ProcessGate() *Gate { return processGate }

const ProcessQueueTimeout = 30 * time.Second
const MaxProcessOutputBytes = 32 * MiB
const MaxForegroundProcessTime = 10 * time.Minute

type ProcessLimitError struct{ Phase string }

func (e *ProcessLimitError) Error() string {
	switch e.Phase {
	case "queue":
		return "外部进程资源等待超过30秒，本次未执行"
	case "output":
		return "脚本输出超过32 MiB，已终止；仅保留额度内输出前缀，结果不完整"
	default:
		return "脚本执行超时，已终止；已有输出可能不完整"
	}
}
func (*ProcessLimitError) PreventModelReplay() bool { return true }

// AcquireProcess does not start or detach work. The caller releases after the
// real executor has stopped. A queue timeout is local pressure, not model quota.
func AcquireProcess(ctx context.Context, key string) (release func(), queued bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	waitCtx, cancel := context.WithTimeoutCause(ctx, ProcessQueueTimeout, &ProcessLimitError{Phase: "queue"})
	defer cancel()
	p, err := ProcessGate().Reserve(key)
	if err != nil {
		return nil, false, err
	}
	if err = p.Wait(waitCtx); err != nil {
		p.Release()
		if cause := context.Cause(waitCtx); cause != nil {
			err = cause
		}
		return nil, p.WasQueued(), err
	}
	return p.Release, p.WasQueued(), nil
}

type ProcessResult struct {
	Output   string
	Started  bool
	ExitCode int
}

// RunProcess bounds project-owned foreground commands. Preparation and the
// optional latest-authorization check happen only after admission. The command
// factory MUST use the supplied context. Its cleanup runs after process Wait.
// OS process setup remains local to SERIES; no official SDK source is changed.
func RunProcess(ctx context.Context, key string, timeout time.Duration, recheck func(context.Context) error,
	prepare func(context.Context) (*exec.Cmd, func(), error)) (result ProcessResult, err error) {
	return runProcess(ctx, key, timeout, recheck, prepare, nil, nil, false)
}

// RunProcessStream keeps the same admission/deadline/aggregate output bound,
// but streams to caller-owned bounded consumers instead of retaining all bytes.
// Stdout and stderr stay separate; a valid rg exit=1 is not a match on stderr.
func RunProcessStream(ctx context.Context, key string, timeout time.Duration, recheck func(context.Context) error,
	prepare func(context.Context) (*exec.Cmd, func(), error), stdout, stderr io.Writer) (ProcessResult, error) {
	return runProcess(ctx, key, timeout, recheck, prepare, stdout, stderr, true)
}

func runProcess(ctx context.Context, key string, timeout time.Duration, recheck func(context.Context) error,
	prepare func(context.Context) (*exec.Cmd, func(), error), stdout, stderr io.Writer, stream bool) (result ProcessResult, err error) {
	release, queued, err := AcquireProcess(ctx, key)
	if err != nil {
		return result, err
	}
	defer release()
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if timeout <= 0 || timeout > MaxForegroundProcessTime {
		timeout = MaxForegroundProcessTime
	}
	runCtx, cancelTimeout := context.WithTimeoutCause(ctx, timeout, &ProcessLimitError{Phase: "runtime"})
	defer cancelTimeout()
	runCtx, cancel := context.WithCancelCause(runCtx)
	defer cancel(nil)
	if queued && recheck != nil {
		if err = recheck(runCtx); err != nil {
			return result, err
		}
	}
	if err = runCtx.Err(); err != nil {
		return result, context.Cause(runCtx)
	}
	cmd, cleanup, err := prepare(runCtx)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return result, err
	}
	if cmd == nil {
		return result, fmt.Errorf("external command not configured")
	}
	if err = runCtx.Err(); err != nil {
		return result, context.Cause(runCtx)
	}
	configureProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	output := &processOutput{limit: MaxProcessOutputBytes, cancel: cancel, discard: stream}
	if stream {
		cmd.Stdout, cmd.Stderr = &processStreamWriter{output: output, sink: stdout}, &processStreamWriter{output: output, sink: stderr}
	} else {
		cmd.Stdout, cmd.Stderr = output, output
	}
	if err = cmd.Start(); err != nil {
		return result, err
	}
	result.Started = true
	err = cmd.Wait()
	// Cancel/WaitDelay must not leave inherited stdout children running. On Unix
	// the command owns a fresh process group, so this also cleans shell children.
	cleanupProcess(cmd)
	result.Output = output.String()
	result.ExitCode = -1
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if cause := context.Cause(runCtx); cause != nil {
		return result, cause
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return result, fmt.Errorf("脚本子进程未关闭输出管道，已执行清理，结果可能不完整")
	}
	return result, err
}

type processOutput struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int64
	cancel    context.CancelCauseFunc
	truncated bool
	written   int64
	discard   bool
}

// os/exec copies each output pipe via io.Copy. Reuse a bounded scratch buffer
// instead of allocating another 32 KiB for every tiny foreground command.
var processCopyBuffers = sync.Pool{New: func() any { return new([32 * 1024]byte) }}

func copyProcessOutput(w io.Writer, r io.Reader) (total int64, err error) {
	buf := processCopyBuffers.Get().(*[32 * 1024]byte)
	defer func() { clear(buf[:]); processCopyBuffers.Put(buf) }()
	for {
		n, err := r.Read(buf[:])
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return total, writeErr
			}
			total += int64(n)
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func (w *processOutput) ReadFrom(r io.Reader) (int64, error) { return copyProcessOutput(w, r) }

type processStreamWriter struct {
	output *processOutput
	sink   io.Writer
}

func (w *processStreamWriter) Write(p []byte) (int, error)         { return w.output.write(p, w.sink) }
func (w *processStreamWriter) ReadFrom(r io.Reader) (int64, error) { return copyProcessOutput(w, r) }

func (w *processOutput) Write(p []byte) (int, error) {
	return w.write(p, nil)
}

func (w *processOutput) write(p []byte, sink io.Writer) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	left := w.limit - w.written
	n := len(p)
	accepted := n
	if int64(accepted) > left {
		accepted = int(left)
	}
	if accepted > 0 {
		w.written += int64(accepted)
		if !w.discard {
			_, _ = w.buf.Write(p[:accepted])
		}
		if sink != nil {
			k, err := sink.Write(p[:accepted])
			if err == nil && k != accepted {
				err = io.ErrShortWrite
			}
			if err != nil {
				w.cancel(err)
				return k, err
			}
		}
	}
	if accepted < n {
		w.truncated = true
		w.cancel(&ProcessLimitError{Phase: "output"})
		return n, nil // drain until the process cancellation reaches the writer
	}
	return n, nil
}
func (w *processOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.buf.String()
	if w.truncated && len(out) > 0 {
		start := len(out) - 1
		for start > 0 && len(out)-start < utf8.UTFMax && !utf8.RuneStart(out[start]) {
			start--
		}
		if !utf8.FullRuneInString(out[start:]) {
			out = out[:start]
		}
	}
	return out
}

var _ io.Writer = (*processOutput)(nil)
var _ io.ReaderFrom = (*processOutput)(nil)
