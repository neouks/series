package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/runtimebudget"
)

type ptyOwnerKey struct{}
type ownedPTYTool struct{ actool.CoreTool }

// ShellSessionTools keeps official metadata/permissions and the five-tool API.
// Install at the factory, BEFORE decorators, so Call never bypasses metering,
// defaults, binding or authorization. Only the process backend is host-owned.
func ShellSessionTools() []actool.CoreTool {
	tools := actool.ShellSessionTools()
	for i, t := range tools {
		tools[i] = &ownedPTYTool{CoreTool: t}
	}
	return tools
}

func hasOwnedPTYTools(tools []actool.CoreTool) bool {
	for _, t := range tools {
		for depth := 0; t != nil && depth < 32; depth++ {
			if wrapper, ok := t.(interface{ UnwrapTool() actool.CoreTool }); ok {
				t = wrapper.UnwrapTool()
				continue
			}
			switch v := t.(type) {
			case *ownedPTYTool:
				return true
			case *guardedTool:
				t = v.CoreTool
			case *overriddenTool:
				t = v.CoreTool
			case *processContextTool:
				t = v.CoreTool
			case *interactiveTool:
				t = v.CoreTool
			default:
				t = nil
			}
		}
	}
	return false
}

func (t *ownedPTYTool) Prompt() string {
	if t.Name() != "shell_open" {
		return t.CoreTool.Prompt()
	}
	return t.CoreTool.Prompt() + " SERIES retains a 256 KiB PTY scrollback and stops output above 32 MiB. Omitted output is incomplete. A PTY holds an external-job slot until actual process exit; prompt/completion notices do not release it."
}

func (t *ownedPTYTool) Call(ctx context.Context, raw json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	o, _ := ctx.Value(ptyOwnerKey{}).(*ptyOwner)
	if o == nil {
		return actool.Errorf("interactive shell has no trusted session owner"), nil
	}
	if actool.InteractiveShellDisabled() {
		return actool.Errorf("交互式shell已被全局停用"), nil
	}
	if err := validateInteractiveInput(t.Name(), t.CoreTool.InputSchema(), raw); err != nil {
		return actool.Errorf("invalid interactive shell input: " + err.Error()), nil
	}
	ctx, leave, err := o.enter(ctx)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	defer leave()
	if err := ctx.Err(); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if t.Name() == "shell_open" {
		return o.open(ctx, raw, tc)
	}
	var args struct {
		ID      string   `json:"session_id"`
		View    string   `json:"view"`
		Cursor  *int64   `json:"since_cursor"`
		Lines   int      `json:"max_lines"`
		Bytes   int      `json:"max_bytes"`
		Drain   bool     `json:"drain"`
		Text    string   `json:"text"`
		Submit  bool     `json:"submit"`
		Keys    []string `json:"keys"`
		Hex     []string `json:"hex"`
		Paste   string   `json:"paste"`
		Wait    *bool    `json:"wait"`
		Prompt  string   `json:"prompt_regex"`
		Quiet   int      `json:"quiet_ms"`
		Timeout int      `json:"timeout_ms"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return actool.Errorf("invalid interactive shell input"), nil
	}
	if t.Name() == "shell_list" {
		o.reap()
		o.mu.Lock()
		ids := make([]string, 0, len(o.sessions))
		for id := range o.sessions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		rows := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			s := o.sessions[id]
			s.mu.Lock()
			rows = append(rows, map[string]any{"session_id": id, "command": s.command, "state": s.state, "mode": s.mode, "idle_ms": time.Since(s.lastOut).Milliseconds()})
			s.mu.Unlock()
		}
		o.mu.Unlock()
		return ptyJSON(map[string]any{"sessions": rows})
	}
	s, ok := o.get(args.ID)
	if !ok {
		return actool.Errorf("会话不存在或已过期: " + args.ID), nil
	}
	switch t.Name() {
	case "shell_close":
		s.stop()
		select {
		case <-ctx.Done():
			return actool.Errorf("会话关闭等待已取消；仍保留资源许可直到实际退出"), nil
		case <-s.done:
		}
		s.mu.Lock()
		code := s.exitCode
		s.mu.Unlock()
		o.mu.Lock()
		delete(o.sessions, s.id)
		o.mu.Unlock()
		return ptyJSON(map[string]any{"closed": s.id, "exit_code": code})
	case "shell_read":
		if args.View == "screen" {
			return ptyJSON(s.screen())
		}
		s.mu.Lock()
		from := s.readCursor
		if args.Cursor != nil {
			from = *args.Cursor
		}
		s.mu.Unlock()
		if args.Lines == 0 {
			args.Lines = 200
		}
		if args.Bytes == 0 {
			args.Bytes = 8192
		}
		text, cur, omitted := s.read(from, args.Lines, args.Bytes, args.Drain)
		s.mu.Lock()
		s.readCursor = cur
		row := map[string]any{"view": "stream", "output": text, "cursor": cur, "running": s.exitCode == nil && s.state == "running", "omitted": omitted}
		if s.exitCode != nil {
			row["exit_code"] = *s.exitCode
		}
		if s.reason != "" {
			row["termination_reason"] = s.reason
			row["incomplete"] = true
		}
		s.mu.Unlock()
		return ptyJSON(row)
	case "shell_send":
		if !s.running() {
			return actool.Errorf("会话已结束，用shell_read读取尾部输出或shell_close关闭"), nil
		}
		var payload []byte
		payload = append(payload, args.Text...)
		if args.Submit {
			payload = append(payload, '\r')
		}
		for _, key := range args.Keys {
			b, ok := ptyKey(key)
			if !ok {
				return actool.Errorf("未知按键；请使用文档中的具名键"), nil
			}
			payload = append(payload, b...)
		}
		for _, h := range args.Hex {
			h = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h)), "0x")
			if n, e := strconv.ParseUint(h, 16, 8); e == nil {
				payload = append(payload, byte(n))
			}
		}
		if args.Paste != "" {
			payload = append(payload, "\x1b[200~"...)
			payload = append(payload, args.Paste...)
			payload = append(payload, "\x1b[201~"...)
		}
		if len(payload) == 0 {
			return actool.Errorf("没有可发送的输入"), nil
		}
		s.mu.Lock()
		from := s.total
		s.mu.Unlock()
		// PTY writes are blocking OS calls. Cancellation closes THIS PTY (not
		// other sessions/Workers), which interrupts the write and retains its
		// permit until the independent Wait owner confirms actual exit.
		stop := context.AfterFunc(ctx, s.stop)
		if tc != nil && tc.Emit != nil {
			tc.Emit(actool.ProgressInfo{Message: "正在发送交互输入"})
		}
		_, err := writeOwnedPTY(ctx, s.master, payload)
		stop()
		if err != nil || ctx.Err() != nil {
			return actool.Errorf("交互输入未完成；会话可能已关闭，请读取当前状态"), nil
		}
		if args.Wait != nil && !*args.Wait {
			return ptyJSON(map[string]any{"session_id": s.id, "sent": len(payload), "running": s.running()})
		}
		prompt := s.prompt
		if args.Prompt != "" {
			if re, e := regexp.Compile(args.Prompt); e == nil {
				prompt = re
			}
		}
		by := s.wait(ctx, ptyMS(args.Quiet, 8000), 0, ptyMS(args.Timeout, 120000), prompt)
		text, cur, omitted := s.read(from, 0, 8192, false)
		return ptyJSON(map[string]any{"session_id": s.id, "output": text, "done_by": by, "cursor": cur, "running": s.running(), "omitted": omitted})
	}
	return actool.Errorf("unknown interactive tool"), nil
}

func (o *ptyOwner) open(ctx context.Context, raw json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if !ownedPTYAvailable() {
		return actool.Errorf("interactive PTY is unavailable on this platform"), nil
	}
	var a struct {
		Command string   `json:"command"`
		Mode    string   `json:"mode"`
		Prompt  string   `json:"prompt_regex"`
		Watch   []string `json:"watch"`
		Rows    int      `json:"rows"`
		Cols    int      `json:"cols"`
		Quiet   int      `json:"quiet_ms"`
		Grace   int      `json:"startup_grace_ms"`
	}
	if json.Unmarshal(raw, &a) != nil || a.Command == "" {
		return actool.Errorf("command必填"), nil
	}
	if a.Mode == "" {
		a.Mode = "hands-free"
	}
	switch a.Mode {
	case "hands-free", "interactive", "dispatch", "monitor":
	default:
		return actool.Errorf("mode需为interactive | hands-free | dispatch | monitor"), nil
	}
	var prompt *regexp.Regexp
	var err error
	if a.Prompt != "" {
		prompt, err = regexp.Compile(a.Prompt)
		if err != nil {
			return actool.Errorf("prompt_regex无效"), nil
		}
	}
	var watch []*regexp.Regexp
	if a.Mode == "monitor" {
		if len(a.Watch) == 0 {
			return actool.Errorf("monitor模式必须提供watch"), nil
		}
		for _, p := range a.Watch {
			re, e := regexp.Compile(p)
			if e != nil {
				return actool.Errorf("watch正则无效"), nil
			}
			watch = append(watch, re)
		}
	}
	release, queued, err := runtimebudget.AcquireProcess(ctx, ExternalProcessKey(ctx))
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()
	if queued {
		if err := recheckQueuedTool(ctx, "shell_open", raw, nil); err != nil {
			return actool.Errorf(err.Error()), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	if a.Rows <= 0 {
		a.Rows = 24
	}
	if a.Cols <= 0 {
		a.Cols = 80
	}
	processCtx, cancel := context.WithCancel(o.ctx)
	cmd := exec.CommandContext(processCtx, "bash", "-c", a.Command)
	var env []string
	if tc != nil {
		cmd.Dir = tc.WorkingDir
		env = tc.Env
	}
	cmd.Env = shellProcessEnv(env)
	master, err := startOwnedPTY(cmd, a.Rows, a.Cols)
	if err != nil {
		cancel()
		return actool.Errorf("开启会话失败: " + err.Error()), nil
	}
	s := &ownedPTY{command: a.Command, mode: a.Mode, state: "running", rows: a.Rows, cols: a.Cols, prompt: prompt, watch: watch, fired: make([]bool, len(watch)), cmd: cmd, master: master, cancel: cancel, ctx: processCtx, done: make(chan struct{}), changed: make(chan struct{}, 1), started: time.Now(), lastOut: time.Now(), owner: o}
	o.mu.Lock()
	o.seq++
	s.id = fmt.Sprintf("sh_%s_%d", o.prefix, o.seq)
	o.sessions[s.id] = s
	closing := o.closed
	o.mu.Unlock()
	transferred = true
	go s.waitProcess(release)
	if closing || ctx.Err() != nil {
		s.stop()
		<-s.done
		return actool.Errorf("interactive opening canceled"), nil
	}
	o.reap()
	quiet, grace := ptyMS(a.Quiet, 8000), ptyMS(a.Grace, 15000)
	switch a.Mode {
	case "interactive":
		by := s.wait(ctx, quiet, grace, 120*time.Second, prompt)
		text, cur, _ := s.read(0, 0, 32768, false)
		return ptyJSON(map[string]any{"session_id": s.id, "state": "running", "done_by": by, "output": text, "cursor": cur, "running": s.running()})
	case "dispatch":
		go func() {
			by := s.wait(o.ctx, quiet, grace, 120*time.Second, prompt)
			s.mu.Lock()
			failed := s.reason != ""
			s.mu.Unlock()
			if o.ctx.Err() == nil && !failed {
				o.notify(s, actool.KindBash, actool.TaskCompleted, "本次等待结束("+by+")；是否退出以shell_read.running为准")
			}
		}()
		return ptyJSON(map[string]any{"session_id": s.id, "state": "running", "dispatched": true})
	case "monitor":
		return ptyJSON(map[string]any{"session_id": s.id, "state": "running", "monitoring": len(watch)})
	default:
		return ptyJSON(map[string]any{"session_id": s.id, "state": "running"})
	}
}

func ptyJSON(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf("marshal interactive result failed"), nil
	}
	return actool.Text(string(b)), nil
}
func ptyMS(n, def int) time.Duration {
	if n <= 0 {
		n = def
	}
	return time.Duration(n) * time.Millisecond
}

func ptyKey(key string) (string, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	keys := map[string]string{"enter": "\r", "return": "\r", "tab": "\t", "esc": "\x1b", "escape": "\x1b", "backspace": "\x7f", "space": " ", "up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D", "home": "\x1b[H", "end": "\x1b[F", "pageup": "\x1b[5~", "pagedown": "\x1b[6~", "delete": "\x1b[3~", "insert": "\x1b[2~"}
	if b, ok := keys[key]; ok {
		return b, true
	}
	if len(key) == 6 && strings.HasPrefix(key, "ctrl+") {
		c := key[5]
		if c >= 'a' && c <= 'z' {
			return string([]byte{c & 0x1f}), true
		}
		if c == '[' {
			return "\x1b", true
		}
		if c == '\\' {
			return "\x1c", true
		}
	}
	return "", false
}
