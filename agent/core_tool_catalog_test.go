package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

func TestTieredToolsKeepRoleCapabilitiesAndNativeSchemas(t *testing.T) {
	sets := builtinToolsByAgent()
	for role, want := range map[string][]string{
		"worker":    {"report_finding", "record_fact", "insert_assets", "context_get", "context_search"},
		"planner":   {"add_intent", "prove_goal", "asset_search"},
		"mainagent": {"dispatch_intents", "set_goals", "set_constraints"},
		"goals":     {"set_goals", "set_constraints"},
	} {
		tools, def := OnDemandTools(sets[role], DeferredInfo{}, role)
		reg := actool.NewRegistry(tools...)
		for _, name := range want {
			core, ok := reg.Get(name)
			if !ok || slices.Contains(def.Deferred, name) {
				t.Fatalf("%s lost resident %s", role, name)
			}
			var original actool.CoreTool
			for _, tool := range sets[role] {
				if tool.Name() == name {
					original = tool
				}
			}
			if !reflect.DeepEqual(core.InputSchema(), original.InputSchema()) {
				t.Fatalf("%s changed parameters", name)
			}
			if utf8.RuneCountInString(core.Description()) > 260 || core.Prompt() != "" {
				t.Fatalf("%s retained long docs", name)
			}
			help, _ := reg.Get(toolHelpName)
			raw, _ := json.Marshal(map[string]string{"name": name})
			result, err := help.Call(t.Context(), raw, nil)
			if err != nil || result.IsError || !strings.Contains(result.Flatten(), original.Description()) {
				t.Fatalf("%s lost full help", name)
			}
		}
	}
}

func TestTieredToolsDoNotPromoteExplicitDeferredOrExposeLockedHelp(t *testing.T) {
	base := []actool.CoreTool{actool.NewRead(), actool.Build(actool.Spec{Name: "Bash", Description: "LOCKED"}), actool.Build(actool.Spec{Name: "mcp__hidden__read", Description: "SECRET"})}
	tools, def := OnDemandTools(base, DeferredInfo{Deferred: []string{"Bash", "mcp__hidden__read"}, Unlock: actool.NewUnlockSet()}, "worker")
	if def.Unlock.Has("Bash") || def.Unlock.Has("mcp__hidden__read") {
		t.Fatal("changed explicit gates")
	}
	help, _ := actool.NewRegistry(tools...).Get(toolHelpName)
	for _, name := range []string{"Bash", "mcp__hidden__read", "unbound"} {
		input, _ := json.Marshal(map[string]string{"name": name})
		result, err := help.Call(t.Context(), input, nil)
		if err != nil || !result.IsError || strings.Contains(result.Flatten(), "SECRET") || strings.Contains(result.Flatten(), "LOCKED") {
			t.Fatalf("disclosed unavailable %s", name)
		}
	}
	tools2, def2 := OnDemandTools(tools, def, "worker")
	if !reflect.DeepEqual(def.Deferred, def2.Deferred) || len(tools) != len(tools2) {
		t.Fatal("duplicate help on reassembly")
	}
}

func TestVisibleSkillCatalogKeepsEverySkillScenario(t *testing.T) {
	reg := skill.NewRegistry()
	for i := 0; i < 140; i++ {
		reg.Add(skill.Skill{Name: fmt.Sprintf("skill-%d", i), Description: "中文🙂" + strings.Repeat("介绍", 150), WhenToUse: fmt.Sprintf("SCENARIO_%d", i), Instructions: "BODY_NOT_LOADED"})
	}
	tools, def := OnDemandTools([]actool.CoreTool{NewVisibleSkillTool(reg)}, DeferredInfo{}, "worker")
	if slices.Contains(def.Deferred, "Skill") {
		t.Fatal("Skill directory hidden")
	}
	catalog := actool.NewRegistry(tools...).Schemas()[0].Description
	if !utf8.ValidString(catalog) || strings.Contains(catalog, "BODY_NOT_LOADED") {
		t.Fatal("invalid catalog/body leaked")
	}
	for i := 0; i < 140; i++ {
		if !strings.Contains(catalog, fmt.Sprintf("skill-%d：SCENARIO_%d", i, i)) {
			t.Fatalf("missing skill applicability %d", i)
		}
	}
}

func TestTieredCoreDirectExecutionKeepsDefaultsAndFullHelp(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls, turn := 0, 0
			schema := map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "default": 3, "minimum": 1}}, "required": []string{"limit"}}
			original := DecorateTool(actool.Build(actool.Spec{Name: "Read", Description: "读取文件。", Prompt: "EXTENDED_HELP", Schema: schema, Run: func(_ context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
				calls++
				if string(raw) != `{"limit":3}` {
					t.Errorf("lost defaults: %s", raw)
				}
				return actool.Text("read ok"), nil
			}}), "读取文件。", schema)
			tools, def := OnDemandTools([]actool.CoreTool{original}, DeferredInfo{}, "worker")
			complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				found := false
				for _, schema := range req.Tools {
					found = found || schema.Name == "Read"
					if strings.Contains(schema.Description, "EXTENDED_HELP") {
						t.Error("eager extended help")
					}
				}
				if !found {
					t.Error("core tool missing native schema")
				}
				turn++
				name, input := "Read", `{}`
				if turn == 2 {
					name, input = toolHelpName, `{"name":"Read"}`
				}
				if turn <= 2 {
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(turn), Name: name, Input: json.RawMessage(input)}}}, "tool_use", llm.Usage{}, nil
				}
				raw, _ := json.Marshal(req.Messages)
				if !strings.Contains(string(raw), "EXTENDED_HELP") {
					t.Error("help not discoverable")
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
			}
			opts := agentcore.Options{Tools: tools, DeferredTools: def.Deferred, UnlockSet: def.Unlock, NonStreaming: !stream, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, Deps: sdkTurnDeps(complete)}
			if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestVisibleSkillCatalogDoesNotUnlockUntilInvoked(t *testing.T) {
	unlock := actool.NewUnlockSet()
	reg := skill.NewRegistry(skill.Skill{Name: "browser", Description: "需要浏览器交互时使用", Instructions: "真实流程", MCPs: []string{"browser"}})
	reg.OnInvoke = func(skill.Skill) string { unlock.Add("mcp__browser__open"); return "已解锁" }
	tools, def := OnDemandTools([]actool.CoreTool{NewVisibleSkillTool(reg), actool.Build(actool.Spec{Name: "mcp__browser__open"})}, DeferredInfo{Deferred: []string{"mcp__browser__open"}, Unlock: unlock}, "worker")
	if unlock.Has("mcp__browser__open") || slices.Contains(def.GlobalNames, "mcp__browser__open") {
		t.Fatal("directory unlocked MCP")
	}
	skillTool, _ := actool.NewRegistry(tools...).Get("Skill")
	if result, err := skillTool.Call(t.Context(), json.RawMessage(`{"name":"browser"}`), nil); err != nil || result.IsError || !unlock.Has("mcp__browser__open") {
		t.Fatalf("invocation failed to unlock: %v", err)
	}
}
