package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/google/uuid"
	"github.com/hinshun/vt10x"
	"github.com/neouks/series/runtimebudget"
)

// ptyOwner owns actual process handles. A completion/prompt notification NEVER
// releases admission; only waitProcess (after cmd.Wait and reader cleanup) does.
type ptyOwner struct {
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	closed       bool
	seq          uint64
	prefix       string
	sessions     map[string]*ownedPTY
	pending      map[string]actool.Notification
	omittedNotes uint64
	wg           sync.WaitGroup
	once         sync.Once
	closedDone   chan struct{}
}

type ownedPTY struct {
	mu                               sync.Mutex
	id, command, mode, state, reason string
	rows, cols                       int
	prompt                           *regexp.Regexp
	watch                            []*regexp.Regexp
	fired                            []bool
	cmd                              *exec.Cmd
	master                           *os.File
	cancel                           context.CancelFunc
	ctx                              context.Context
	stopOnce                         sync.Once
	done                             chan struct{}
	changed                          chan struct{}
	buf                              []byte
	total, readCursor                int64
	emu                              vt10x.Terminal
	finalScreen                      map[string]any
	exitCode                         *int
	started, lastOut, exitedAt       time.Time
	owner                            *ptyOwner
}

func newPTYOwner() *ptyOwner {
	ctx, cancel := context.WithCancel(context.Background())
	return &ptyOwner{ctx: ctx, cancel: cancel, prefix: strings.ReplaceAll(uuid.NewString(), "-", ""), sessions: map[string]*ownedPTY{}, pending: map[string]actool.Notification{}, closedDone: make(chan struct{})}
}

func (o *ptyOwner) enter(ctx context.Context) (context.Context, func(), error) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil, nil, fmt.Errorf("interactive session is closed")
	}
	o.wg.Add(1)
	o.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.ctx, cancel)
	return ctx, func() { stop(); cancel(); o.wg.Done() }, nil
}

func (o *ptyOwner) close() {
	if err := o.closeWait(5 * time.Second); err != nil {
		log.Printf("[resources] %v", err)
	}
}

func (o *ptyOwner) closeWait(timeout time.Duration) error {
	o.once.Do(func() {
		o.mu.Lock()
		o.closed = true
		o.cancel()
		sessions := make([]*ownedPTY, 0, len(o.sessions))
		for _, s := range o.sessions {
			sessions = append(sessions, s)
		}
		o.mu.Unlock()
		for _, s := range sessions {
			s.stop()
		}
		go func() {
			// Cancel first: queued openings and blocked writes must drain. Keep
			// watching after a caller timeout; never release a live job's slot.
			o.wg.Wait()
			o.mu.Lock()
			for _, s := range o.sessions {
				sessions = append(sessions, s)
			}
			o.mu.Unlock()
			for _, s := range sessions {
				s.stop()
				<-s.done
			}
			o.mu.Lock()
			o.sessions = map[string]*ownedPTY{}
			o.pending = map[string]actool.Notification{}
			o.mu.Unlock()
			close(o.closedDone)
		}()
	})
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-o.closedDone:
		return nil
	case <-timer.C:
		return fmt.Errorf("PTY cleanup still awaits actual process exit; resource slots remain held")
	}
}

func (o *ptyOwner) get(id string) (*ownedPTY, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	s, ok := o.sessions[id]
	return s, ok
}

func (o *ptyOwner) reap() {
	now := time.Now()
	o.mu.Lock()
	var ended []*ownedPTY
	for id, s := range o.sessions {
		s.mu.Lock()
		exited := !s.exitedAt.IsZero()
		age := now.Sub(s.exitedAt)
		idle := now.Sub(s.lastOut)
		s.mu.Unlock()
		if exited {
			if age > 5*time.Minute {
				delete(o.sessions, id)
			} else {
				ended = append(ended, s)
			}
		} else if idle > 30*time.Minute {
			s.stop()
		}
	}
	// Retain at most 32 exited rings. Active processes have their own Gate limit.
	sort.Slice(ended, func(i, j int) bool { return ended[i].exitedAt.Before(ended[j].exitedAt) })
	for len(ended) > 32 {
		delete(o.sessions, ended[0].id)
		ended = ended[1:]
	}
	o.mu.Unlock()
}

func (o *ptyOwner) notify(s *ownedPTY, kind actool.TaskKind, status actool.TaskStatus, why string) {
	// One pending notice per session coalesces pattern bursts. No output,
	// credentials or full command is copied into the notification workset.
	n := actool.Notification{TaskID: s.id, Kind: kind, Status: status, Summary: fmt.Sprintf("交互会话 %s：%s。用 shell_read(session_id=%q) 查看真实输出。", s.id, why, s.id)}
	o.mu.Lock()
	if !o.closed {
		if _, exists := o.pending[s.id]; !exists && len(o.pending) >= 128 {
			var oldest string
			for id := range o.pending {
				if oldest == "" || id < oldest {
					oldest = id
				}
			}
			delete(o.pending, oldest)
			o.omittedNotes++
		}
		o.pending[s.id] = n
	}
	o.mu.Unlock()
}

func (o *ptyOwner) drain() []actool.Notification {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	ids := make([]string, 0, len(o.pending))
	for id := range o.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]actool.Notification, 0, len(ids))
	if o.omittedNotes > 0 {
		rows = append(rows, actool.Notification{TaskID: "pty-notices", Kind: actool.KindMonitor, Status: actool.TaskRunning, Summary: fmt.Sprintf("%d条较早交互通知未展示；用shell_list与shell_read核对当前状态，未展示不代表没有事件。", o.omittedNotes)})
		o.omittedNotes = 0
	}
	for _, id := range ids {
		rows = append(rows, o.pending[id])
		delete(o.pending, id)
	}
	return rows
}

func (s *ownedPTY) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode == nil && s.state == "running"
}
func (s *ownedPTY) stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.state = "closed"
		s.mu.Unlock()
		s.cancel()
		killPTYProcess(s.cmd, s.master)
		_ = s.master.Close()
	})
}

func (s *ownedPTY) append(p []byte) {
	s.mu.Lock()
	remaining := max(int64(0), int64(runtimebudget.MaxProcessOutputBytes)-s.total)
	overflow := int64(len(p)) > remaining
	if overflow {
		p = p[:remaining]
		s.reason = "output_limit"
	}
	s.total += int64(len(p))
	s.buf = append(s.buf, p...)
	if len(s.buf) > ptyRingBytes {
		s.buf = s.buf[len(s.buf)-ptyRingBytes:]
	}
	if s.emu != nil {
		_, _ = s.emu.Write(p)
	}
	s.lastOut = time.Now()
	var hit bool
	if len(s.watch) > 0 {
		tail := s.buf[max(0, len(s.buf)-8192):]
		for i, re := range s.watch {
			if !s.fired[i] && re.Match(tail) {
				s.fired[i] = true
				hit = true
			}
		}
	}
	s.mu.Unlock()
	if overflow {
		s.owner.notify(s, actool.KindMonitor, actool.TaskFailed, "输出超过32MiB，已终止；环内输出不完整")
		s.stop()
	}
	if hit && !overflow {
		s.owner.notify(s, actool.KindMonitor, actool.TaskRunning, "监听条件命中，进程仍运行")
	}
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *ownedPTY) waitProcess(release func()) {
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		b := make([]byte, 32768)
		for {
			n, err := readOwnedPTY(s.ctx, s.master, b)
			if n > 0 {
				s.append(b[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	_ = s.cmd.Wait() // the sole owner of Wait, independent of PTY EOF/status
	killPTYProcess(s.cmd, s.master)
	// A fast process can exit with unread bytes still in the PTY. Let the
	// reader drain first, but do not hang on a descendant retaining the slave.
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		s.mu.Lock()
		if s.reason == "" {
			s.reason = "output_drain_incomplete"
		}
		s.mu.Unlock()
		_ = s.master.Close()
		<-readDone
	}
	_ = s.master.Close()
	s.cancel()
	s.mu.Lock()
	code := -1
	if s.cmd.ProcessState != nil {
		code = s.cmd.ProcessState.ExitCode()
	}
	s.exitCode = &code
	reason := s.reason
	s.exitedAt = time.Now()
	if s.emu != nil {
		s.finalScreen = s.screenLocked(false)
		s.emu = nil
	}
	if s.state != "closed" {
		s.state = "exited"
	}
	s.mu.Unlock()
	release()
	close(s.done)
	if s.mode == "monitor" && reason == "" {
		s.owner.notify(s, actool.KindMonitor, actool.TaskCompleted, fmt.Sprintf("进程已退出(exit %d)", code))
	}
	s.owner.reap()
}

func (s *ownedPTY) read(from int64, lines, bytes int, drain bool) (string, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.total - int64(len(s.buf))
	omitted := from < base
	if from < base {
		from = base
	}
	if from >= s.total {
		return "", s.total, false
	}
	seg := s.buf[from-base:]
	if omitted {
		for len(seg) > 0 && !utf8.RuneStart(seg[0]) {
			seg = seg[1:]
			from++
		}
	}
	if !drain {
		if bytes > 0 && len(seg) > bytes {
			n := bytes
			for n > 0 && !utf8.RuneStart(seg[n]) {
				n--
			}
			seg = seg[:n]
		}
		if lines > 0 {
			count := 0
			for i, b := range seg {
				if b == '\n' {
					count++
					if count == lines {
						seg = seg[:i+1]
						break
					}
				}
			}
		}
	}
	// Preserve an incomplete trailing rune for the next incremental read.
	if len(seg) > 0 {
		start := len(seg) - 1
		for start > 0 && len(seg)-start < utf8.UTFMax && !utf8.RuneStart(seg[start]) {
			start--
		}
		if !utf8.FullRune(seg[start:]) {
			seg = seg[:start]
		}
	}
	return string(seg), from + int64(len(seg)), omitted
}

func (s *ownedPTY) screen() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalScreen != nil {
		row := make(map[string]any, len(s.finalScreen))
		for k, v := range s.finalScreen {
			row[k] = v
		}
		return row
	}
	if s.emu == nil {
		s.emu = vt10x.New(vt10x.WithSize(s.cols, s.rows))
		_, _ = s.emu.Write(s.buf)
	}
	return s.screenLocked(s.exitCode == nil && s.state == "running")
}

func (s *ownedPTY) screenLocked(running bool) map[string]any {
	var out strings.Builder
	for y := 0; y < s.rows; y++ {
		var line strings.Builder
		for x := 0; x < s.cols; x++ {
			r := s.emu.Cell(x, y).Char
			if r == 0 {
				r = ' '
			}
			line.WriteRune(r)
		}
		out.WriteString(strings.TrimRight(line.String(), " "))
		out.WriteByte('\n')
	}
	cursor := s.emu.Cursor()
	row := map[string]any{"view": "screen", "screen": out.String(), "rows": s.rows, "cols": s.cols, "cursor": map[string]int{"row": cursor.Y, "col": cursor.X}, "running": running}
	if s.reason != "" {
		row["termination_reason"] = s.reason
		row["incomplete"] = true
	}
	return row
}

func (s *ownedPTY) wait(ctx context.Context, quiet, grace, timeout time.Duration, prompt *regexp.Regexp) string {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		last := s.lastOut
		tail := string(s.buf[max(0, len(s.buf)-4096):])
		s.mu.Unlock()
		if prompt != nil && prompt.MatchString(tail) {
			return "prompt"
		}
		due := last.Add(quiet)
		if initial := s.cmdStart().Add(grace); due.Before(initial) {
			due = initial
		}
		delay := time.Until(due)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-s.done:
			timer.Stop()
			return "exited"
		case <-ctx.Done():
			timer.Stop()
			return "timeout"
		case <-deadline.C:
			timer.Stop()
			return "timeout"
		case <-s.changed:
			timer.Stop()
		case <-timer.C:
			return "quiet"
		}
	}
}

func (s *ownedPTY) cmdStart() time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.started }
