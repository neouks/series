package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
	"github.com/neouks/series/guard"
)

type sdkPolicyProbe struct {
	pre, post      int
	name           string
	input, rewrite []byte
	block          bool
}

func (h *sdkPolicyProbe) PreToolUse(_ context.Context, name string, input []byte) (bool, string, []byte) {
	h.pre++
	h.name = name
	h.input = append([]byte(nil), input...)
	return h.block, "policy denied", h.rewrite
}
func (h *sdkPolicyProbe) PostToolUse(_ context.Context, name string, input, _ []byte, _ bool) {
	h.post++
	h.name = name
	h.input = append([]byte(nil), input...)
}
func (*sdkPolicyProbe) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

func sdkToolTurn(name, input string) harness.QueryDeps {
	step := 0
	complete := func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		step++
		if step == 1 {
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "source-call", Name: name, Input: json.RawMessage(input)}}}, "tool_use", llm.Usage{}, nil
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
	}
	return harness.QueryDeps{CallModelSync: complete, CallModel: func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			m, stop, _, _ := complete(ctx, req)
			for _, b := range m.Content {
				if b.Type == llm.BlockToolUse {
					if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
						return
					}
					if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
						return
					}
				} else if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
					return
				}
			}
			if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop}, nil) {
				return
			}
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}}
}

func TestOfficialSessionDeferredPolicyAndSource(t *testing.T) {
	for _, stream := range []bool{true, false} {
		for _, tc := range []struct {
			name                   string
			direct, locked, deny   bool
			target, input, rewrite string
			wantCalls              int
		}{
			{name: "defaults", wantCalls: 1},
			{name: "direct", direct: true, wantCalls: 1},
			{name: "rewrite", rewrite: `{"limit":5}`, wantCalls: 1},
			{name: "invalid rewrite", rewrite: `{"limit":51}`},
			{name: "invalid input", input: `{"limit":51}`},
			{name: "locked", locked: true},
			{name: "direct locked", direct: true, locked: true},
			{name: "denied", deny: true},
			{name: "unbound", target: "not-bound"},
			{name: "nested", target: actool.ExecuteExtraToolName},
			{name: "discovery bypass", target: actool.SearchExtraToolsName},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				probe := &sdkPolicyProbe{block: tc.deny, rewrite: []byte(tc.rewrite)}
				calls := 0
				tool := DecorateTool(actool.Build(actool.Spec{Name: "insert_assets", Run: func(_ context.Context, in json.RawMessage, ctx *actool.ToolContext) (actool.Result, error) {
					calls++
					if ctx.ToolUseID != "source-call" {
						t.Errorf("source id lost: %q", ctx.ToolUseID)
					}
					if string(in) != `{"limit":3}` && string(in) != `{"limit":5}` {
						t.Errorf("defaults/rewrite not executed: %s", in)
					}
					return actool.Text("ok"), nil
				}}), "", map[string]any{"type": "object", "required": []string{"limit"}, "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "default": 3}}})
				target := tc.target
				if target == "" {
					target = tool.Name()
				}
				params := tc.input
				if params == "" {
					params = `{}`
				}
				name, input := target, params
				if !tc.direct {
					name = actool.ExecuteExtraToolName
					input = fmt.Sprintf(`{"tool_name":%q,"params":%s}`, target, params)
				}
				unlock := actool.NewUnlockSet()
				if !tc.locked {
					unlock.Add(tool.Name())
				}
				opts := agentcore.Options{Tools: []actool.CoreTool{guardPanic(tool)}, DeferredTools: []string{tool.Name()}, UnlockSet: unlock, Hooks: probe,
					DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, NonStreaming: !stream, Deps: sdkToolTurn(name, input)}
				var events []db.Activity
				_, _, err := captureRun(t.Context(), opts, "go", func(a db.Activity) { events = append(events, a) })
				if err != nil {
					t.Fatal(err)
				}
				if calls != tc.wantCalls {
					t.Fatalf("calls=%d want=%d: %+v", calls, tc.wantCalls, events)
				}
				if tc.wantCalls > 0 {
					if probe.pre != 1 || probe.post != 1 || probe.name != tool.Name() {
						t.Fatalf("policy duplicated or wrapper leaked: %+v", probe)
					}
					for _, a := range events {
						if a.Kind == "tool_use" || a.Kind == "tool_result" {
							if a.Tool != "insert_assets" || a.ToolUseID != "source-call" {
								t.Fatalf("source not locatable: %+v", a)
							}
						}
					}
				} else if len(events) < 2 || !events[1].IsError {
					t.Fatalf("missing refusal: %+v", events)
				}
			})
		}
	}
}

func TestDeferredAssetPolicyCannotBypassPlannerOrRewrites(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("sdk-policy", "test", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	_, err = d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: "pending.sdk.test", TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	target := actool.NewBash()
	for _, role := range []string{"mainagent", "planner"} {
		probe := &sdkPolicyProbe{}
		opts := agentcore.Options{Tools: []actool.CoreTool{target}, DeferredTools: []string{"Bash"}, UnlockSet: actool.NewUnlockSet("Bash"), PermissionMode: permission.ModeBypass,
			Hooks: guard.AssetPolicyHooks(probe, d.Assets(), task.ID)}
		hooks := newDispatchHooks(opts)
		ctx := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: role})
		input := []byte(`{"tool_name":"Bash","params":{"command":"curl https://pending.sdk.test"}}`)
		blocked, reason, _ := hooks.PreToolUse(ctx, actool.ExecuteExtraToolName, input)
		if blocked != (role == "planner") {
			t.Fatalf("%s blocked=%v reason=%s", role, blocked, reason)
		}
		// A benign original command rewritten to a forbidden pending target must
		// still fail Planner policy, without invoking action approval twice.
		if role == "planner" {
			probe.rewrite = []byte(`{"command":"curl https://pending.sdk.test"}`)
			blocked, _, _ = hooks.PreToolUse(ctx, actool.ExecuteExtraToolName, []byte(`{"tool_name":"Bash","params":{"command":"printf harmless"}}`))
			if !blocked || probe.pre != 1 {
				t.Fatalf("rewritten target bypassed policy: %+v", probe)
			}
		}
	}
}

func TestInteractiveManagerWithoutWorkerBackgroundTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("official interactive PTY is unavailable on Windows")
	}
	t.Setenv("AGENT_CORE_DISABLE_INTERACTIVE_SHELL", "")
	tools := []actool.CoreTool{actool.NewShellOpen(), actool.NewShellClose(), actool.NewShellList()}
	s, closeSession, err := newSession(agentcore.Options{Tools: tools, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, NonStreaming: true,
		Deps: sdkToolTurn("shell_open", `{"command":"cat"}`)})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession()
	var result string
	for ev, err := range s.Prompt(t.Context(), "open") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == harness.KindToolResult {
			result = blocksText(ev.ToolResult.Content)
			if ev.ToolResult.IsError {
				t.Fatal(result)
			}
		}
	}
	if !strings.Contains(result, "session_id") {
		t.Fatal(result)
	}
	closeSession() // cleanup must be safe even after caller cancellation/close.
}

func TestOfficialDeferredInsertPersistsExactOrigin(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			task, err := d.CreateTask("sdk origin", "goal", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer d.DeleteTask(task.ID)
			store := d.Exploration(task.ExplorationID)
			ts := NewToolSet(store, "human")
			ts.SetTaskID(task.ID)
			ts.SetAssetStore(d.Assets(), d.Companies())
			input := fmt.Sprintf(`{"tool_name":"insert_assets","params":{"assets":[{"type":"subdomain","domain":"sdk-origin-%d.test"}]}}`, task.ID)
			opts := agentcore.Options{Tools: []actool.CoreTool{ts.insertAssets()}, DeferredTools: []string{"insert_assets"},
				DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, NonStreaming: !stream, Deps: sdkToolTurn(actool.ExecuteExtraToolName, input)}
			ctx := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, ExplorationID: task.ExplorationID, AgentKey: "mainagent"})
			seg := 2
			var commandID int64
			_, _, err = captureRun(ctx, opts, "register", func(a db.Activity) {
				a.Worker = "mainagent"
				a.MainSeg = &seg
				id, e := store.AppendActivity(a)
				if e != nil {
					t.Error(e)
				}
				if a.Kind == "tool_use" {
					commandID = id
				}
				if a.Kind == "tool_result" && a.IsError {
					t.Error(a.Detail)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err = d.QueryRow(`SELECT source_origin FROM task_asset_links WHERE task_id=$1 AND source_origin IS NOT NULL LIMIT 1`, task.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var origin db.AssetOrigin
			if err = json.Unmarshal(raw, &origin); err != nil {
				t.Fatal(err)
			}
			if commandID == 0 || origin.ActivityID != commandID || origin.ToolUseID != "source-call" || origin.Session != "main:2" {
				t.Fatalf("origin=%+v command=%d", origin, commandID)
			}
		})
	}
}

func TestHostInteractiveCleanupDrainsAndRejectsLateCalls(t *testing.T) {
	manager, err := actool.NewManager("host-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	owner := newInteractiveOwner(manager)
	defer owner.close()
	started := make(chan struct{})
	finished := make(chan struct{})
	tool := &interactiveTool{owner: owner, CoreTool: actool.Build(actool.Spec{Name: "shell_open", Run: func(ctx context.Context, _ json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
		if tc.Tasks != manager {
			t.Error("wrong manager")
		}
		close(started)
		<-ctx.Done()
		close(finished)
		return actool.Errorf(ctx.Err().Error()), nil
	}})}
	go tool.Call(t.Context(), json.RawMessage(`{}`), nil)
	<-started
	done := make(chan struct{})
	go func() { owner.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not cancel/drain call")
	}
	select {
	case <-finished:
	default:
		t.Fatal("cleanup returned before call settled")
	}
	result, err := tool.Call(t.Context(), json.RawMessage(`{}`), nil)
	if err != nil || !result.IsError {
		t.Fatal("late call admitted")
	}
}
