package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestOnDemandToolsPreserveGatesAndResolvedHelp(t *testing.T) {
	unlock := actool.NewUnlockSet("global", "removed")
	base := []actool.CoreTool{
		actool.Build(actool.Spec{Name: "builtin", Description: "edited description", Prompt: "extended help", Schema: obj(nil)}),
		actool.Build(actool.Spec{Name: "global"}), actool.Build(actool.Spec{Name: "gated"}),
	}
	tools, def := OnDemandTools(base, DeferredInfo{Deferred: []string{"global", "gated", "removed"}, GlobalNames: []string{"global", "removed", "global"}, Unlock: unlock})
	if !reflect.DeepEqual(def.Deferred, []string{"builtin", "gated", "global"}) || !reflect.DeepEqual(def.GlobalNames, []string{"builtin", "global"}) {
		t.Fatalf("wrong names: %+v", def)
	}
	if def.Unlock != unlock || unlock.Has("gated") || !unlock.Has("builtin") {
		t.Fatal("lost or bypassed unlock gate")
	}
	search := actool.NewSearchExtraTools(actool.NewRegistry(tools...), def.Deferred)
	result, err := search.Call(t.Context(), json.RawMessage(`{"query":"select:builtin"}`), nil)
	if err != nil || !strings.Contains(result.Flatten(), "edited description\n\nextended help") || tools[0].Prompt() != "" {
		t.Fatalf("discovery lost help: %s %v", result.Flatten(), err)
	}
	_, again := OnDemandTools(tools, def)
	if !reflect.DeepEqual(again.Deferred, def.Deferred) || !reflect.DeepEqual(again.GlobalNames, def.GlobalNames) || unlock.Has("gated") {
		t.Fatal("assembly is not idempotent")
	}
}

func TestOnDemandSessionDiscoveryAndSkillLoading(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, hosted := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/hosted=%v", stream, hosted), func(t *testing.T) {
				calls, turn := 0, 0
				reg := skill.NewRegistry(skill.Skill{Name: "sample", Description: "SKILL_DIRECTORY_DETAIL", Instructions: "SKILL_BODY_DETAIL"})
				tool := actool.Build(actool.Spec{Name: "echo", Description: "TOOL_DESCRIPTION_DETAIL", Prompt: "TOOL_EXTENDED_DETAIL", Schema: obj(nil), Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
					calls++
					return actool.Text("executed"), nil
				}})
				tools, def := OnDemandTools([]actool.CoreTool{tool, NewVisibleSkillTool(reg)}, DeferredInfo{})
				system, boundary := deferredSystem("test", def)
				steps := []struct{ name, input string }{
					{actool.SearchExtraToolsName, `{"query":"select:echo"}`},
					{"Skill", `{"name":"sample"}`},
					{actool.ExecuteExtraToolName, `{"tool_name":"echo","params":{}}`},
				}
				complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					for _, schema := range req.Tools {
						if schema.Name == "echo" || strings.Contains(schema.Description, "SKILL_BODY_DETAIL") || strings.Contains(schema.Description, "TOOL_DESCRIPTION_DETAIL") {
							t.Error("full catalog leaked into request tools")
						}
					}
					raw, _ := json.Marshal(req)
					if turn == 0 && (strings.Contains(string(raw), "TOOL_DESCRIPTION_DETAIL") || strings.Contains(string(raw), "SKILL_BODY_DETAIL") || !strings.Contains(string(raw), "SKILL_DIRECTORY_DETAIL")) {
						t.Error("unused descriptions/body loaded eagerly")
					}
					if turn == 1 && (!strings.Contains(string(raw), "TOOL_EXTENDED_DETAIL") || !strings.Contains(string(raw), "SKILL_DIRECTORY_DETAIL") || strings.Contains(string(raw), "SKILL_BODY_DETAIL")) {
						t.Error("discovery failed to load only requested definitions")
					}
					if turn >= 2 && !strings.Contains(string(raw), "SKILL_BODY_DETAIL") {
						t.Error("skill invocation body missing")
					}
					if turn < len(steps) {
						step := steps[turn]
						turn++
						return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(turn), Name: step.name, Input: json.RawMessage(step.input)}}}, "tool_use", llm.Usage{}, nil
					}
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
				}
				opts := agentcore.Options{Tools: tools, DeferredTools: def.Deferred, UnlockSet: def.Unlock, SystemPrompt: system, DynamicBoundary: boundary, DisableBackgroundTasks: true, NonStreaming: !stream, PermissionMode: permission.ModeBypass, Deps: sdkTurnDeps(complete)}
				if hosted {
					opts.Transcript = transcript.NewStore(t.TempDir())
				}
				if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("executions=%d", calls)
				}
			})
		}
	}
}

func TestOnDemandSettlementStillBlocksExecution(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/direct=%v", stream, direct), func(t *testing.T) {
				calls, turn := 0, 0
				tool := actool.Build(actool.Spec{Name: "Bash", Schema: obj(nil), Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
					calls++
					return actool.Text("executed"), nil
				}})
				tools, def := OnDemandTools([]actool.CoreTool{tool}, DeferredInfo{Deferred: []string{"Bash"}, GlobalNames: []string{"Bash"}})
				complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					for _, schema := range req.Tools {
						if schema.Name == settlementMarkerName {
							t.Fatal("internal phase marker leaked")
						}
					}
					turn++
					if turn <= 3 {
						name, input := actool.ExecuteExtraToolName, `{"tool_name":"Bash","params":{}}`
						if direct {
							name, input = "Bash", `{}`
						}
						if turn == 1 {
							name, input = actool.SearchExtraToolsName, `{"query":"select:Bash"}`
						}
						return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(turn), Name: name, Input: json.RawMessage(input)}}}, "tool_use", llm.Usage{}, nil
					}
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
				}
				opts := agentcore.Options{Tools: tools, DeferredTools: def.Deferred, UnlockSet: def.Unlock, NonStreaming: !stream, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, MaxTurns: 2,
					Settlement: &harness.Settlement{Prompt: "finish", DisabledTools: []string{"Bash"}, MaxTurns: 3}, Deps: sdkTurnDeps(complete)}
				if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("settlement bypass: calls=%d", calls)
				}
				if slices.Contains(opts.Settlement.DisabledTools, settlementMarkerName) {
					t.Fatal("mutated caller settlement")
				}
			})
		}
	}
}
