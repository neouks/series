package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
	"github.com/neouks/series/guard"
)

// Exercise the real SDK execution loop; a schema disappearing alone is not an
// execution boundary if the model guesses a historical tool name.
func TestSDKReviewSettlementCannotExecuteHiddenBash(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls, turns := 0, 0
			tool := actool.Build(actool.Spec{Name: "Bash", Schema: obj(nil), Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				calls++
				return actool.Text("executed"), nil
			}})
			complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				turns++
				if turns <= 2 {
					visible := false
					for _, schema := range req.Tools {
						visible = visible || schema.Name == "Bash"
					}
					if visible != (turns == 1) {
						t.Errorf("turn %d visibility=%v", turns, visible)
					}
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(turns), Name: "Bash", Input: json.RawMessage(`{}`)}}}, "tool_use", llm.Usage{}, nil
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("settled")}}, "end_turn", llm.Usage{}, nil
			}
			opts := agentcore.Options{Tools: []actool.CoreTool{tool}, NonStreaming: !stream, PermissionMode: permission.ModeBypass, DisableBackgroundTasks: true,
				MaxTurns: 1, Settlement: &harness.Settlement{Prompt: "write back only", DisabledTools: []string{"Bash"}, MaxTurns: 3}, Deps: reviewModelDeps(complete)}
			_, reason, err := captureRun(t.Context(), opts, "go", nil)
			if err != nil || reason != harness.ReasonMaxTurns || calls != 1 {
				t.Fatalf("calls=%d turns=%d reason=%s err=%v", calls, turns, reason, err)
			}
		})
	}
}

func reviewModelDeps(complete func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error)) harness.QueryDeps {
	return harness.QueryDeps{CallModelSync: complete, CallModel: func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			m, stop, usage, err := complete(ctx, req)
			if err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
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
			if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage}, nil) {
				return
			}
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}}
}

func TestSDKReviewEmptyBindingDoesNotRestoreDefaultTools(t *testing.T) {
	opts := agentcore.Options{DisableBackgroundTasks: true, NonStreaming: true, Deps: reviewModelDeps(func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if len(req.Tools) != 0 {
			t.Errorf("empty binding restored %d SDK tools", len(req.Tools))
		}
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
	})}
	if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
		t.Fatal(err)
	}
}

type reviewApprovalHook struct {
	sdkPolicyProbe
	afterApproval func()
}

func (h *reviewApprovalHook) PreToolUse(ctx context.Context, name string, in []byte) (bool, string, []byte) {
	h.afterApproval()
	return h.sdkPolicyProbe.PreToolUse(ctx, name, in)
}

func TestSDKReviewRechecksAssetsAfterUnchangedApproval(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprint(deferred), func(t *testing.T) {
			task, err := d.CreateTaskWithOptions("sdk stale approval", "fixture", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
			if err != nil {
				t.Fatal(err)
			}
			defer d.DeleteTask(task.ID)
			_, err = d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: "revoked.sdk-review.test", TaskID: task.ID, AgentDiscovered: true})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := d.Assets().ListTaskAssetApprovals(task.ID)
			if err != nil || len(rows) == 0 {
				t.Fatalf("approvals=%v err=%v", rows, err)
			}
			var ids []int64
			for _, row := range rows {
				ids = append(ids, row.AssetID)
			}
			if err := d.Assets().ApproveTaskAssets(task.ID, ids, "test", ""); err != nil {
				t.Fatal(err)
			}
			probe := &reviewApprovalHook{afterApproval: func() {
				if err := d.Assets().RevokeTaskAssets(task.ID, ids, "test", "revoked while awaiting approval"); err != nil {
					t.Fatal(err)
				}
			}}
			opts := agentcore.Options{Tools: []actool.CoreTool{actool.NewBash()}, PermissionMode: permission.ModeBypass, Hooks: guard.AssetPolicyHooks(probe, d.Assets(), task.ID)}
			name, input := "Bash", `{"command":"curl https://revoked.sdk-review.test"}`
			if deferred {
				opts.DeferredTools, opts.UnlockSet = []string{"Bash"}, actool.NewUnlockSet("Bash")
				name, input = actool.ExecuteExtraToolName, `{"tool_name":"Bash","params":`+input+`}`
			}
			ctx := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "mainagent"})
			blocked, reason, _ := newDispatchHooks(opts).PreToolUse(ctx, name, []byte(input))
			if !blocked || probe.pre != 1 || !strings.Contains(reason, "资产") {
				t.Fatalf("blocked=%v approvals=%d reason=%s", blocked, probe.pre, reason)
			}
		})
	}
}

func TestSDKReviewNoaTruncationCannotLeakUnverifiableAssetBody(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("noa truncated authorization", "fixture", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	id, err := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: "truncated.sdk-review.test", TaskID: task.ID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	opts := agentcore.Options{Compaction: &compaction.Config{ContextWindow: 40000}}
	enableNoa(&opts, func() bool { return true }, t.TempDir(), "sdk-review", nil)
	var history []llm.Message
	for i := 0; i < 10; i++ {
		callID := fmt.Sprint(i)
		body := fmt.Sprintf(`{"assets":[{"id":%d,"type":"root_domain","domain":"truncated.sdk-review.test","detail":"SECRET_REVOKED_EVIDENCE %s"}]}`, id, strings.Repeat("evidence ", 8000))
		history = append(history,
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: callID, Name: "asset_get", Input: json.RawMessage(fmt.Sprintf(`{"ids":[%d],"offset":%d}`, id, i))}}},
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(callID, body, false)}})
	}
	before, _ := json.Marshal(history)
	view := opts.Compactor.(harness.ContextView).View(t.Context(), history)
	// The SDK valve cuts a JSON result in the middle. Its head still contains
	// protected content, but the ordinary JSON authorization parser cannot read it.
	cut := false
	for _, m := range view {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && strings.Contains(blocksText(b.Content), "SECRET_REVOKED_EVIDENCE") && strings.Contains(blocksText(b.Content), "original ~") {
				cut = true
			}
		}
	}
	if !cut {
		t.Fatal("fixture did not exercise official mechanical truncation")
	}
	if err := d.Assets().RevokeTaskAssets(task.ID, []int64{id}, "test", "withdrawn"); err != nil {
		t.Fatal(err)
	}
	ctx := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "planner"})
	out, err := FilterTaskAssetRequest(ctx, d.Assets(), task.ID, llm.CompletionRequest{Messages: view})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out.Messages)
	if strings.Contains(string(raw), "SECRET_REVOKED_EVIDENCE") {
		t.Fatal("truncated JSON bypassed authorization refresh")
	}
	after, _ := json.Marshal(history)
	if string(before) != string(after) {
		t.Fatal("raw history modified")
	}
}
