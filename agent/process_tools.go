package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
	"github.com/neouks/series/runtimebudget"
)

// Keep the SDK's schema, permission floor and interpreter contract. The host
// owns admission, not a fork of the SDK. Install before decorators/meters so
// direct and deferred calls still traverse the same configured tool exactly once.
func newAdmittedBash() actool.CoreTool { return &admittedBash{CoreTool: actool.NewBash()} }

type admittedBash struct{ actool.CoreTool }

// Only the stateless permission method is reused; runtime schema/environment
// discovery still happens when constructing each actual role tool.
var officialBashFloor = actool.NewBash()

func (t *admittedBash) Description() string {
	return "Runs a non-interactive shell command in the working directory and returns combined stdout/stderr. Use dedicated file tools for file work. Background execution requires the session task manager; returned jobs retain their resource slot until actual exit."
}

func (t *admittedBash) Prompt() string {
	return t.CoreTool.Prompt() + " SERIES shares a bounded external-job queue; a returned background task keeps its slot until the SDK confirms exit. Without a task manager, execution is limited to 10 minutes and 32 MiB output; incomplete output is not a complete conclusion."
}

func (t *admittedBash) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return RunAdmittedBash(ctx, input, tc, nil)
}

func admittedDefaultTools() []actool.CoreTool {
	tools := actool.DefaultTools()
	for i, tool := range tools {
		if tool.Name() == "Bash" {
			tools[i] = newAdmittedBash()
		}
		if tool.Name() == "Grep" {
			tools[i] = newAdmittedGrep()
		}
	}
	return tools
}

// ExternalProcessKey groups all host launch paths by trusted run attribution.
// It is not an authorization decision and cannot be supplied by tool arguments.
func ExternalProcessKey(ctx context.Context) string {
	run := RunInfoFrom(ctx)
	if run.TaskID > 0 {
		return fmt.Sprintf("task:%d", run.TaskID)
	}
	if run.SessionID != "" {
		return "session:" + run.SessionID
	}
	if session := transcript.SessionIDFrom(ctx); session != "" {
		return "session:" + session
	}
	return "external-unscoped"
}

type processRecheckKey struct{}
type processRecheck func(context.Context, string, []byte) string

// recheckQueuedProcess runs authorization only, never action approval/counting.
// Rebinding the read context also makes queued Worker access writes cancelable.
func recheckQueuedProcess(ctx context.Context, input json.RawMessage, explicit func(context.Context) error) error {
	return recheckQueuedTool(ctx, "Bash", input, explicit)
}

func recheckQueuedTool(ctx context.Context, name string, input json.RawMessage, explicit func(context.Context) error) error {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if explicit != nil {
		return explicit(checkCtx)
	}
	if check, ok := ctx.Value(processRecheckKey{}).(processRecheck); ok {
		if reason := check(checkCtx, name, input); reason != "" {
			return fmt.Errorf("%s", reason)
		}
		return checkCtx.Err()
	}
	if RunInfoFrom(ctx).TaskID > 0 {
		return fmt.Errorf("queued task command has no trusted authorization recheck; not executed")
	}
	return checkCtx.Err()
}

type admittedMonitor struct{ actool.CoreTool }

func newAdmittedMonitor() actool.CoreTool { return &admittedMonitor{CoreTool: actool.NewMonitor()} }
func (t *admittedMonitor) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return actool.Result{}, err
	}
	if strings.TrimSpace(in.Command) == "" {
		return actool.Errorf("Error: command is required"), nil
	}
	if decision := t.CheckPermissions(ctx, input, permission.Context{}); decision.Behavior == permission.Deny {
		return actool.Errorf(decision.Message), nil
	}
	if tc == nil || tc.Tasks == nil {
		return actool.Errorf("Error: background tasks are not enabled"), nil
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
		if err = recheckQueuedTool(ctx, "Monitor", input, nil); err != nil {
			return actool.Errorf(err.Error()), nil
		}
	}
	if err = ctx.Err(); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	// Preserve the official Monitor environment contract (unlike Bash, Monitor
	// does not receive tc.Env). Its manager/watchdogs/notifications stay official.
	task, err := tc.Tasks.Spawn(actool.SpawnSpec{Command: in.Command, Description: in.Description, Kind: actool.KindMonitor, WorkingDir: tc.WorkingDir})
	if err != nil {
		return actool.Errorf("Error: failed to start monitor: " + err.Error()), nil
	}
	watchAdmittedJob(tc.Tasks, task.ID, release)
	transferred = true
	tc.Tasks.Background(task.ID)
	return actool.Text(fmt.Sprintf("Started monitor %s. Output file: %s\nUse TaskOutput{task_id:%q} or Read to inspect it; TaskStop{task_id:%q} to end it.", task.ID, task.OutputPath, task.ID, task.ID)), nil
}

// RunAdmittedBash is also used by custom command tools after their own template
// rendering/approval. The rendered command is checked again after queuing.
func RunAdmittedBash(ctx context.Context, input json.RawMessage, tc *actool.ToolContext, recheck func(context.Context) error) (actool.Result, error) {
	var in struct {
		Command    string `json:"command"`
		TimeoutMS  int64  `json:"timeout_ms"`
		Background bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return actool.Result{}, err
	}
	if strings.TrimSpace(in.Command) == "" {
		return actool.Errorf("Error: command is required"), nil
	}
	// Nested custom-command execution does not pass the SDK permission pipeline
	// a second time. Still enforce its hard floor; do not ask for approval twice.
	if decision := officialBashFloor.CheckPermissions(ctx, input, permission.Context{}); decision.Behavior == permission.Deny {
		return actool.Errorf(decision.Message), nil
	}
	ms := in.TimeoutMS
	if ms <= 0 {
		ms = 120000
	}
	if ms > 600000 {
		ms = 600000
	} // clamp before duration conversion (no overflow)
	timeout := time.Duration(ms) * time.Millisecond
	check := func(c context.Context) error { return recheckQueuedProcess(c, input, recheck) }
	if tc != nil && tc.Tasks != nil && !actool.BackgroundTasksDisabled() {
		return runAdmittedManagedBash(ctx, tc, in.Command, timeout, in.Background, check)
	}
	result, err := runtimebudget.RunProcess(ctx, ExternalProcessKey(ctx), timeout, check, func(c context.Context) (*exec.Cmd, func(), error) {
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(c, "powershell", "-NoProfile", "-NonInteractive", "-Command", in.Command)
		} else {
			cmd = exec.CommandContext(c, "bash", "-c", in.Command)
		}
		var extra []string
		if tc != nil {
			cmd.Dir, extra = tc.WorkingDir, tc.Env
		}
		cmd.Env = shellProcessEnv(extra)
		return cmd, nil, nil
	})
	out := result.Output
	if err != nil {
		return actool.Errorf(actool.Capture(tc, out+"\n\n[command incomplete: "+err.Error()+"]")), nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(command produced no output)"
	}
	return actool.Text(actool.Capture(tc, out)), nil
}

// One waiter per admitted job, not model polling. Only Wait's completion flag
// proves cmd.Wait has finished: TaskKilled can be published before process exit.
// The SDK retains the task until our foreground reader Forget, or session Close.
type admittedManagedJob struct {
	done chan struct{}
	info actool.TaskInfo // published by closing done
}

func watchAdmittedJob(manager *actool.Manager, id string, release func()) *admittedManagedJob {
	job := &admittedManagedJob{done: make(chan struct{})}
	go func() {
		for {
			if info, exited := manager.Wait(context.Background(), id, 24*time.Hour); exited {
				job.info = info
				release()
				close(job.done)
				return
			}
			// Do not infer exit from a missing/terminal snapshot. This path is
			// conservative on unexpected external Forget; avoid a busy loop.
			time.Sleep(time.Second)
		}
	}()
	return job
}

func runAdmittedManagedBash(ctx context.Context, tc *actool.ToolContext, command string, timeout time.Duration, background bool, check func(context.Context) error) (actool.Result, error) {
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
		if err = check(ctx); err != nil {
			return actool.Errorf(err.Error()), nil
		}
	}
	if err = ctx.Err(); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	task, err := tc.Tasks.Spawn(actool.SpawnSpec{Command: command, Kind: actool.KindBash, WorkingDir: tc.WorkingDir, Description: shortProcessCommand(command), Env: tc.Env})
	if err != nil {
		return actool.Errorf("Error: failed to start command: " + err.Error()), nil
	}
	job := watchAdmittedJob(tc.Tasks, task.ID, release)
	transferred = true
	backgroundResult := func(prefix string) (actool.Result, error) {
		tc.Tasks.Background(task.ID)
		return actool.Text(fmt.Sprintf("%s as %s. Output file: %s\nUse TaskOutput{task_id:%q} or Read to read its output; you are notified when it completes.", prefix, task.ID, task.OutputPath, task.ID)), nil
	}
	if background {
		return backgroundResult("Running in background")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-job.done:
		out, readErr := tc.Tasks.Output(task.ID, 0)
		tc.Tasks.Forget(task.ID) // waiter has observed real exit; no stranded permit
		if readErr != nil {
			return actool.Errorf("command output unavailable: " + readErr.Error()), nil
		}
		if job.info.Status != actool.TaskCompleted {
			return actool.Errorf(actool.Capture(tc, out) + fmt.Sprintf("\n\n[command incomplete: status=%s exit=%d reason=%s]", job.info.Status, job.info.ExitCode, job.info.Reason)), nil
		}
		if strings.TrimSpace(out) == "" {
			out = "(command produced no output)"
		}
		return actool.Text(actool.Capture(tc, out)), nil
	case <-ctx.Done():
		return stopAdmittedBash(tc, task.ID, job, ctx.Err().Error())
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			return stopAdmittedBash(tc, task.ID, job, err.Error())
		}
		fields := strings.Fields(command)
		base := strings.ReplaceAll(fields[0], "\\", "/")
		base = base[strings.LastIndex(base, "/")+1:]
		if base != "sleep" {
			return backgroundResult(fmt.Sprintf("Command still running after %s; moved to background", timeout))
		}
		return stopAdmittedBash(tc, task.ID, job, fmt.Sprintf("timed out after %s", timeout))
	}
}

func stopAdmittedBash(tc *actool.ToolContext, id string, job *admittedManagedJob, reason string) (actool.Result, error) {
	tc.Tasks.Kill(id)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-job.done:
		out, _ := tc.Tasks.Output(id, 0)
		tc.Tasks.Forget(id)
		return actool.Errorf(actool.Capture(tc, out) + "\n\n[command incomplete: " + reason + "]"), nil
	case <-timer.C:
		// The waiter keeps the permit; never equate a stop request with exit.
		return actool.Errorf(fmt.Sprintf("command stop requested (%s); exit not yet confirmed, resource slot retained; TaskOutput{task_id:%q}", reason, id)), nil
	}
}

func shortProcessCommand(command string) string {
	command = strings.ReplaceAll(strings.TrimSpace(command), "\n", " ")
	if len(command) <= 60 {
		return command
	}
	n := 57
	for n > 0 && !utf8.RuneStart(command[n]) {
		n--
	}
	return command[:n] + "..."
}

// Same precedence as the official default shell: inherited < noninteractive
// defaults < per-run overrides (notably proxy/CA). No global PATH mutation.
func shellProcessEnv(extra []string) []string {
	defaults := []string{"PAGER=cat", "GIT_PAGER=cat", "SYSTEMD_PAGER=cat", "DEBIAN_FRONTEND=noninteractive"}
	out := make([]string, 0, len(os.Environ())+len(defaults)+len(extra))
	positions := map[string]int{}
	for _, layer := range [][]string{os.Environ(), defaults, extra} {
		for _, kv := range layer {
			key, _, ok := strings.Cut(kv, "=")
			if !ok || key == "" {
				out = append(out, kv) // retain e.g. Windows' inherited =C: entries
				continue
			}
			if runtime.GOOS == "windows" {
				key = strings.ToUpper(key)
			}
			if i, ok := positions[key]; ok {
				out[i] = kv
			} else {
				positions[key] = len(out)
				out = append(out, kv)
			}
		}
	}
	return out
}
