package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestShellUsageCommands(t *testing.T) {
	rows := []*db.Tool{{Key: "scan", Executable: "nmap", Kind: "shell", Enabled: true, Agents: []string{"worker"}}, {Key: "fetch", Executable: "/opt/bin/curl", Kind: "shell", Enabled: true, Agents: []string{"worker"}}, {Key: "hidden", Executable: "ffuf", Kind: "shell", Enabled: true, Agents: []string{"planner"}}}
	for _, tt := range []struct {
		command string
		want    []string
	}{
		{"nmap a; nmap b | /opt/bin/curl x", []string{"fetch", "scan"}},
		{"X=1 env -u HOME A=b sudo -u root command -- nmap a", []string{"scan"}},
		{"echo nmap; # nmap a\n echo 'nmap b'; f() { nmap c; }; bash -c 'nmap d'", nil},
		{"$tool a; ${tool} b; $(printf nmap) c", nil},
		{"false && nmap a", []string{"scan"}},
		{"'/opt/bin/curl' x; command -v nmap; ffuf x", []string{"fetch"}},
		{"nmap '$(bad)'", []string{"scan"}},
		{"nmap '", nil},
	} {
		t.Run(tt.command, func(t *testing.T) {
			got, _ := shellUsageKeys(tt.command, rows, "worker")
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
	rows = append(rows, &db.Tool{Key: "duplicate", Executable: "nmap", Kind: "shell", Enabled: true, Agents: []string{"worker"}})
	if got, conflicts := shellUsageKeys("nmap a", rows, "worker"); len(got) != 0 || len(conflicts) != 1 {
		t.Fatal(got, conflicts)
	}
}

func TestShellUsageDirectoryAttribution(t *testing.T) {
	rows := []*db.Tool{
		{Key: "gogo", Executable: "gogo_linux_amd64", Directory: "/root/pentestools", Kind: "shell", Enabled: true, Agents: []string{"worker"}},
		{Key: "elsewhere", Executable: "gogo_linux_amd64", Directory: "/opt/other", Kind: "shell", Enabled: true, Agents: []string{"worker"}},
		{Key: "disabled", Executable: "other", Directory: "/root/pentestools", Kind: "shell", Agents: []string{"worker"}},
		{Key: "unbound", Executable: "extra", Directory: "/root/pentestools", Kind: "shell", Enabled: true, Agents: []string{"planner"}},
	}
	for _, tt := range []struct {
		name, command string
		want          []string
	}{
		{"background example", "cd /root/artex/data/tasks/123/i14278 && /root/pentestools/gogo_linux_amd64 -i 221.2.93.248 -p 1-65535 --http -j -o gogo_full.json --timeout 3 --retry 1 2>&1 | tail -50", []string{"gogo"}},
		{"quoted path", "'/root/pentestools/gogo_linux_amd64' -i target", []string{"gogo"}},
		{"same basename elsewhere", "/opt/other/gogo_linux_amd64 target", []string{"elsewhere"}},
		{"unconfigured program in directory", "/root/pentestools/unknown target", nil},
		{"conditional request", "false && /root/pentestools/gogo_linux_amd64 target", []string{"gogo"}},
		{"repeated in one request", "/root/pentestools/gogo_linux_amd64 a; /root/pentestools/gogo_linux_amd64 b", []string{"gogo"}},
		{"not executable positions", "echo '/root/pentestools/gogo_linux_amd64'; # /root/pentestools/gogo_linux_amd64\nf() { /root/pentestools/gogo_linux_amd64; }; bash -c '/root/pentestools/gogo_linux_amd64'", nil},
		{"disabled and unbound", "/root/pentestools/other; /root/pentestools/extra", nil},
		{"invalid shell", "'/root/pentestools/gogo_linux_amd64", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, conflicts := shellUsageKeys(tt.command, rows, "worker")
			if !reflect.DeepEqual(got, tt.want) || len(conflicts) != 0 {
				t.Fatalf("got %v, conflicts %v; want %v", got, conflicts, tt.want)
			}
		})
	}
	if got, conflicts := shellUsageKeys("gogo_linux_amd64 target", rows, "worker"); len(got) != 0 || len(conflicts) != 1 {
		t.Fatalf("ambiguous bare command counted: %v, %v", got, conflicts)
	}
	if got, conflicts := shellUsageKeys("gogo_linux_amd64 target", rows[:1], "worker"); !reflect.DeepEqual(got, []string{"gogo"}) || len(conflicts) != 0 {
		t.Fatalf("unique bare command not counted: %v, %v", got, conflicts)
	}
	relative := []*db.Tool{{Key: "scan", Executable: "scan_linux", Directory: "pentestools", Kind: "shell", Enabled: true, Agents: []string{"worker"}}}
	if got, _ := shellUsageKeys("/data/pentestools/scan_linux target", relative, "worker", "/data"); !reflect.DeepEqual(got, []string{"scan"}) {
		t.Fatalf("relative configured directory not resolved: %v", got)
	}

	// A configured directory must not steal another declaration's absolute path.
	rows = append(rows, &db.Tool{Key: "duplicate", Executable: "/root/pentestools/gogo_linux_amd64", Kind: "shell", Enabled: true, Agents: []string{"worker"}})
	if got, conflicts := shellUsageKeys("/root/pentestools/gogo_linux_amd64 target", rows, "worker"); len(got) != 0 || len(conflicts) != 1 {
		t.Fatalf("ambiguous command counted: %v, %v", got, conflicts)
	}
}

func TestShellUsageMeterAndFailure(t *testing.T) {
	recorder := &recordingToolUsage{}
	base := actool.Build(actool.Spec{Name: "Bash", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		return actool.Errorf("failed command"), nil
	}})
	ri := agent.RunInfo{TaskID: 7, ExplorationID: 8, IntentID: 9, SessionID: "s"}
	tool := &shellMeteredTool{CoreTool: meterTool(base, recorder, "Bash", "worker", ri), recorder: recorder, declarations: []*db.Tool{{Key: "gogo", Executable: "gogo_linux_amd64", Directory: "/root/pentestools", Kind: "shell", Enabled: true, Agents: []string{"worker"}}}, agentKey: "worker", ri: ri}
	r, e := tool.Call(t.Context(), json.RawMessage(`{"command":"cd /root/artex/data/tasks/123/i14278 && /root/pentestools/gogo_linux_amd64 -i 221.2.93.248 -p 1-65535 --http -j -o gogo_full.json --timeout 3 --retry 1 2>&1 | tail -50; /root/pentestools/gogo_linux_amd64 --version","run_in_background":true}`), nil)
	if e != nil || !r.IsError || len(recorder.rows) != 2 {
		t.Fatal(r, e, recorder.rows)
	}
	if recorder.rows[0].ToolKey != "gogo" || recorder.rows[0].SessionID != "s" || recorder.rows[0].TaskID != 7 || recorder.rows[0].IntentID != 9 || recorder.rows[1].ToolKey != "Bash" {
		t.Fatal(recorder.rows)
	}
}

func TestShellUsageValidationAndLedgerFailure(t *testing.T) {
	recorder := &recordingToolUsage{}
	executed := 0
	base := actool.Build(actool.Spec{Name: "Bash", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		executed++
		return actool.Text("ok"), nil
	}})
	tool := &shellMeteredTool{CoreTool: meterTool(base, recorder, "Bash", "worker", agent.RunInfo{}), recorder: recorder, declarations: []*db.Tool{{Key: "gogo", Executable: "gogo_linux_amd64", Directory: "/root/pentestools", Kind: "shell", Enabled: true, Agents: []string{"worker"}}}, agentKey: "worker"}
	schema := map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []any{"command"}}
	validated := agent.DecorateTool(tool, "", schema)
	if result, err := validated.Call(t.Context(), json.RawMessage(`{"timeout_ms":100}`), nil); err != nil || !result.IsError || len(recorder.rows) != 0 || executed != 0 {
		t.Fatalf("invalid request entered execution: result=%+v err=%v rows=%v executed=%d", result, err, recorder.rows, executed)
	}
	recorder.err = errors.New("ledger unavailable")
	if result, err := validated.Call(t.Context(), json.RawMessage(`{"command":"/root/pentestools/gogo_linux_amd64 target"}`), nil); err != nil || result.IsError || executed != 1 {
		t.Fatalf("metering failure interrupted execution: result=%+v err=%v executed=%d", result, err, executed)
	}
}

func TestShellUsagePersistedAttribution(t *testing.T) {
	s, task := modeServer(t)
	key := "test_shell_usage_" + task.ID
	declaration := &db.Tool{Key: key, Executable: "gogo_linux_amd64", Directory: "/root/pentestools", Kind: "shell", Enabled: true, Agents: []string{"worker"}}
	if err := s.m.pg.CreateCustomTool(declaration); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.m.pg.Exec(`DELETE FROM tool_usage WHERE tool_key=$1 OR session_id=$2`, key, "shell-usage-"+task.ID)
		_ = s.m.pg.DeleteCustomTool(key)
	})
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	sessionID := "shell-usage-" + task.ID
	ri := agent.RunInfo{TaskID: taskID, ExplorationID: task.ExpID, AgentKey: "worker", SessionID: sessionID}
	base := actool.Build(actool.Spec{Name: "Bash", Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		return actool.Text("submitted"), nil
	}})
	tool := &shellMeteredTool{CoreTool: meterTool(base, s.m.pg, "Bash", "worker", ri), recorder: s.m.pg, declarations: []*db.Tool{declaration}, agentKey: "worker", ri: ri}
	command := fmt.Sprintf(`{"command":"cd /root/artex/data/tasks/%s/i14278 && /root/pentestools/gogo_linux_amd64 -i 221.2.93.248 2>&1 | tail -50","run_in_background":true}`, task.ID)
	if result, err := tool.Call(t.Context(), json.RawMessage(command), nil); err != nil || result.IsError {
		t.Fatal(result, err)
	}
	var customCalls, bashCalls int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM tool_usage WHERE tool_key=$1 AND session_id=$2 AND task_id=$3 AND exploration_id=$4 AND agent_key='worker'`, key, sessionID, taskID, task.ExpID).Scan(&customCalls); err != nil {
		t.Fatal(err)
	}
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM tool_usage WHERE tool_key='Bash' AND session_id=$1`, sessionID).Scan(&bashCalls); err != nil {
		t.Fatal(err)
	}
	if customCalls != 1 || bashCalls != 1 {
		t.Fatalf("persisted custom/Bash calls = %d/%d, want 1/1", customCalls, bashCalls)
	}
	counts, err := s.m.pg.ToolUsageCounts()
	if err != nil || counts[key] != 1 {
		t.Fatalf("catalog calls = %d, err = %v", counts[key], err)
	}
}

func TestCustomToolDirectAndDeferredUsage(t *testing.T) {
	t.Setenv("AGENT_CORE_DISABLE_INTERACTIVE_SHELL", "1")
	s, _ := modeServer(t)
	old := agent.ToolResolve
	oldFinding := agent.FindingTrafficBindingEnabled
	defer func() { agent.ToolResolve = old; agent.FindingTrafficBindingEnabled = oldFinding }()
	wireTools(s.m.pg, nil)
	for _, kind := range []string{"command", "script", "http"} {
		key := "usage_probe_" + kind
		s.m.pg.DeleteCustomTool(key)
		if err := s.m.pg.CreateCustomTool(&db.Tool{Key: key, Kind: kind, Enabled: true, Deferred: true, Agents: []string{"worker"}, Description: "decorated"}); err != nil {
			t.Fatal(err)
		}
		defer s.m.pg.DeleteCustomTool(key)
		defer s.m.pg.Exec(`DELETE FROM tool_usage WHERE tool_key=$1`, key)
		calls := 0
		base := actool.Build(actool.Spec{Name: key, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			calls++
			return actool.Text("ok"), nil
		}})
		resolved, err := agent.ToolResolve(t.Context(), "worker", []actool.CoreTool{base})
		if err != nil || len(resolved) != 1 {
			t.Fatal(err, resolved)
		}
		if _, err := resolved[0].Call(t.Context(), json.RawMessage(`{}`), nil); err != nil {
			t.Fatal(err)
		}
		reg := actool.NewRegistry(resolved[0])
		extra := actool.NewExecuteExtraTool(reg, nil)
		raw, _ := json.Marshal(map[string]any{"tool_name": key, "params": map[string]any{}})
		result, err := extra.Call(t.Context(), raw, &actool.ToolContext{ExecuteTool: func(ctx context.Context, name string, input json.RawMessage) (actool.Result, error) {
			return resolved[0].Call(ctx, input, nil)
		}})
		if err != nil || result.IsError {
			t.Fatal(result, err)
		}
		counts, err := s.m.pg.ToolUsageCounts()
		if err != nil || counts[key] != 2 || calls != 2 {
			t.Fatal(counts[key], calls, err)
		}
		// Missing policy dispatcher must not reach the metered inner tool.
		extra.Call(t.Context(), raw, nil)
		counts, _ = s.m.pg.ToolUsageCounts()
		if counts[key] != 2 {
			t.Fatal("denied deferred call counted")
		}
	}
}
