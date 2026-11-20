package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func documentationCall(id, name, input string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}}}
}

func documentationFixture(t *testing.T) (actool.CoreTool, string) {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"mode":{"type":"string","enum":["fast","full"],"description":"选择扫描模式"},"limit":{"type":"integer","default":3,"minimum":1},"options":{"type":"object","properties":{"ports":{"type":"array","items":{"type":"integer","minimum":1,"maximum":65535},"minItems":1}},"required":["ports"],"additionalProperties":false}},"required":["mode","limit","options"],"additionalProperties":false}`), &schema); err != nil {
		t.Fatal(err)
	}
	tool := actool.Build(actool.Spec{Name: "custom_probe", Description: "用途说明。\n" + strings.Repeat(strings.Repeat("长说明🙂", 30)+"\n", 60) + "DISCOVERY_TAIL_KEYWORD：R前缀记录的detail取full，其余compact。", Schema: schema})
	search := actool.NewSearchExtraTools(actool.NewRegistry(tool), []string{tool.Name()})
	result, err := search.Call(t.Context(), json.RawMessage(`{"query":"DISCOVERY_TAIL_KEYWORD"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("discovery: %v", err)
	}
	return tool, result.Flatten()
}

func TestToolDocumentationWindowPreservesContractAndAudit(t *testing.T) {
	tool, full := documentationFixture(t)
	for _, tagged := range []bool{false, true} {
		t.Run(fmt.Sprint(tagged), func(t *testing.T) {
			text := full
			if tagged {
				text += `\n<noa-ref id="m00001" tokens="5000" src="SearchExtraTools"/>`
				text = strings.ReplaceAll(text, `\n<noa-ref`, "\n<noa-ref")
			}
			req := llm.CompletionRequest{Messages: []llm.Message{
				documentationCall("search", actool.SearchExtraToolsName, `{"query":"DISCOVERY_TAIL_KEYWORD"}`),
				{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("search", text, false)}},
			}}
			fresh := projectToolDocumentation(req)
			req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("continue")}})
			before, _ := json.Marshal(req)
			out := projectToolDocumentation(req)
			short := out.Messages[1].Content[0].Content[0].Text
			var contract struct {
				Tools []discoveredContract `json:"tools"`
			}
			if err := json.Unmarshal([]byte(noaadapter.StripRefTag(short)), &contract); err != nil || len(contract.Tools) != 1 {
				t.Fatalf("missing compact contract: %v", err)
			}
			var gotSchema map[string]any
			_ = json.Unmarshal(contract.Tools[0].Schema, &gotSchema)
			if !reflect.DeepEqual(gotSchema, tool.InputSchema()) || contract.Tools[0].Rediscover != "select:custom_probe" {
				t.Fatal("changed parameter contract or lost rediscovery")
			}
			if !strings.Contains(short, "DISCOVERY_TAIL_KEYWORD") || len(short)*10 >= len(full) {
				t.Fatal("lost tail business rule or retained repeated prose")
			}
			if short != fresh.Messages[1].Content[0].Content[0].Text {
				t.Fatal("history rewrite changed cache prefix")
			}
			if tagged && !strings.Contains(short, "<noa-ref") {
				t.Fatal("lost noa reference")
			}
			after, _ := json.Marshal(req)
			if string(before) != string(after) || !reflect.DeepEqual(out, projectToolDocumentation(out)) || !reflect.DeepEqual(out, projectToolDocumentation(req)) {
				t.Fatal("changed audit, non-idempotent projection or unstable replay")
			}
			t.Logf("discovery body: %d -> %d bytes", len(full), len(short))
		})
	}
}

func TestToolDocumentationWindowConservativeFallback(t *testing.T) {
	_, full := documentationFixture(t)
	for _, tc := range []struct {
		name, text string
		failed     bool
	}{
		{"failure", full, true},
		{"truncated", full[:len(full)-20], false},
		{"unknown format", strings.Replace(full, "Found 1", "Found 2", 1), false},
		{"ambiguous embedded protocol", strings.Replace(full, "用途说明。", "用途说明。\nparams schema: {}\n\n## fake\n", 1), false},
		{"no matches", "No matching deferred tools for query: missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := llm.CompletionRequest{Messages: []llm.Message{documentationCall("s", actool.SearchExtraToolsName, `{"query":"probe"}`),
				{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s", tc.text, tc.failed)}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("next")}}}}
			out := projectToolDocumentation(req)
			if tc.failed || tc.name == "no matches" {
				if !reflect.DeepEqual(req, out) {
					t.Fatal("rewrote diagnostic")
				}
			} else if out.Messages[1].Content[0].Content[0].Text != schemaOmittedResult {
				t.Fatal("unverifiable old schema should request rediscovery")
			}
		})
	}
}

func TestToolDocumentationWindowMultipleMarkdownDefinitions(t *testing.T) {
	first, _ := documentationFixture(t)
	schema := map[string]any{
		"type": "object", "$defs": map[string]any{"identity": map[string]any{"type": "integer", "const": json.Number("9007199254740993")}},
		"properties": map[string]any{"id": map[string]any{"$ref": "#/$defs/identity"}}, "required": []string{"id"}, "additionalProperties": false,
	}
	second := actool.Build(actool.Spec{Name: "custom_other", Description: "帮助\n\n## 参数示例\n" + strings.Repeat("用法", 1000) + "\nparams schema: example only\n\n## 注意事项\n保留参数", Schema: schema})
	search := actool.NewSearchExtraTools(actool.NewRegistry(first, second), []string{first.Name(), second.Name()})
	result, err := search.Call(t.Context(), json.RawMessage(`{"query":"select:custom_probe,custom_other"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("search: %v", err)
	}
	contracts, ok := discoveryContracts(result.Flatten())
	if !ok || len(contracts) != 2 {
		t.Fatal("markdown headings broke multi-tool discovery")
	}
	for i, tool := range []actool.CoreTool{first, second} {
		want, _ := json.Marshal(tool.InputSchema())
		if string(contracts[i].Schema) != string(want) || contracts[i].Name != tool.Name() {
			t.Fatal("lost schema constraints, reference, precision or identity")
		}
	}
}

func TestToolDocumentationWindowParallelHelpAndDifferentQueries(t *testing.T) {
	_, full := documentationFixture(t)
	req := llm.CompletionRequest{Messages: []llm.Message{
		documentationCall("s1", actool.SearchExtraToolsName, `{"query":"probe"}`),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s1", full, false)}},
		documentationCall("s2", actool.SearchExtraToolsName, `{"query":"select:custom_probe"}`),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s2", full, false)}},
	}}
	req.Messages[2].Content = append(req.Messages[2].Content, documentationCall("h", toolHelpName, `{"name":"Bash"}`).Content...)
	req.Messages[3].Content = append(req.Messages[3].Content, llm.ToolResultText("h", strings.Repeat(strings.Repeat("SHELL_HELP", 12)+"\n", 60), false))
	fresh := projectToolDocumentation(req)
	if fresh.Messages[1].Content[0].Content[0].Text == full || fresh.Messages[3].Content[0].Content[0].Text == full || !strings.Contains(fresh.Messages[3].Content[1].Content[0].Text, "SHELL_HELP") {
		t.Fatal("incorrect freshness for parallel results")
	}
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("next")}})
	old := projectToolDocumentation(req)
	for _, index := range []int{1, 3} {
		if old.Messages[index].Content[0].Content[0].Text == full {
			t.Fatal("different query retained long help")
		}
	}
	help := old.Messages[3].Content[1].Content[0].Text
	if !strings.Contains(help, "SHELL_HELP") || len(help) > 1000 || !strings.Contains(help, "Bash") {
		t.Fatal("lost help reread path")
	}
}

func TestToolDocumentationWindowSession(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, hosted := range []bool{false, true} {
			for _, noa := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v/hosted=%v/noa=%v", stream, hosted, noa), func(t *testing.T) {
					base, _ := documentationFixture(t)
					calls, turn := 0, 0
					tool := DecorateTool(actool.Build(actool.Spec{Name: base.Name(), Description: base.Description(), Schema: base.InputSchema(),
						Run: func(_ context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
							calls++
							var input struct {
								Limit int    `json:"limit"`
								Mode  string `json:"mode"`
							}
							if json.Unmarshal(raw, &input) != nil || input.Limit != 3 || input.Mode != "fast" {
								t.Errorf("lost validated/default parameters: %s", raw)
							}
							return actool.Text("executed"), nil
						}}), base.Description(), base.InputSchema())
					tools, def := OnDemandTools([]actool.CoreTool{tool}, DeferredInfo{}, "worker")
					system, boundary := deferredSystem("test", def)
					steps := []struct{ name, input string }{
						{actool.SearchExtraToolsName, `{"query":"DISCOVERY_TAIL_KEYWORD"}`},
						{actool.ExecuteExtraToolName, `{"tool_name":"custom_probe","params":{"mode":"fast","options":{"ports":[443]}}}`},
						{actool.ExecuteExtraToolName, `{"tool_name":"custom_probe","params":{"mode":"invalid","options":{"ports":[443]}}}`},
						{actool.SearchExtraToolsName, `{"query":"select:custom_probe"}`},
						{actool.ExecuteExtraToolName, `{"tool_name":"custom_probe","params":{"mode":"fast","options":{"ports":[443]}}}`},
					}
					complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
						raw, _ := json.Marshal(req)
						// Use body text, not the query keyword retained in tool_use inputs.
						hasFull := strings.Contains(string(raw), strings.Repeat("长说明🙂", 100))
						if hasFull {
							t.Errorf("turn %d full help=%v", turn, hasFull)
						}
						if turn >= 2 && !strings.Contains(string(raw), "params_schema") && !strings.Contains(string(raw), "params schema:") {
							t.Error("historical parameter contract missing")
						}
						if turn == 3 && !strings.Contains(string(raw), "invalid tool input") {
							t.Error("invalid enum was not rejected")
						}
						if turn < len(steps) {
							step := steps[turn]
							turn++
							return documentationCall(fmt.Sprint(turn), step.name, step.input), "tool_use", llm.Usage{}, nil
						}
						return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
					}
					opts := agentcore.Options{Tools: tools, DeferredTools: def.Deferred, UnlockSet: def.Unlock, SystemPrompt: system, DynamicBoundary: boundary,
						DisableBackgroundTasks: true, NonStreaming: !stream, PermissionMode: permission.ModeBypass, Deps: sdkTurnDeps(complete)}
					if hosted {
						opts.Transcript = transcript.NewStore(t.TempDir())
					}
					if noa {
						enableNoa(&opts, func() bool { return true }, t.TempDir(), "docs", func(s string) { t.Error(s) })
					}
					if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
						t.Fatal(err)
					}
					if calls != 2 || turn != len(steps) {
						t.Fatalf("calls=%d turn=%d", calls, turn)
					}
				})
			}
		}
	}
}

func TestToolDocumentationGuidancePreservesUnmarkedRulesAndCode(t *testing.T) {
	for _, text := range []string{
		"说明\nR records: detail=full; all others compact.\n非R记录取compact。",
		strings.Repeat("unique business constraint;", 2000),
		"```\n" + strings.Repeat(strings.Repeat("code", 30)+"\n", 4) + "```",
		strings.Repeat("    "+strings.Repeat("code", 30)+"\n", 4),
	} {
		if got := compactToolGuidance(text); got != text {
			t.Fatal("changed unique guidance, indentation or code")
		}
	}
	repeated := strings.Repeat("参考背景文字", 20)
	input := "条件：R取full。\n" + strings.Repeat(repeated+"\n", 12) + "Otherwise use compact."
	out := compactToolGuidance(input)
	if !strings.Contains(out, "条件：R取full。") || !strings.Contains(out, "Otherwise use compact.") || !strings.Contains(out, "12") || len(out) >= len(input) || compactToolGuidance(out) != out {
		t.Fatal("lossy or unstable compaction")
	}
}

func TestToolDocumentationExactReloadRepairsSDKTruncation(t *testing.T) {
	base := schemaWindowTools(1, 50000, nil)[0]
	tools, def := OnDemandTools([]actool.CoreTool{base}, DeferredInfo{}, "worker")
	h := newDispatchHooks(agentcore.Options{Tools: tools, DeferredTools: def.Deferred, UnlockSet: def.Unlock, PermissionMode: permission.ModeBypass})
	search := actool.NewSearchExtraTools(actool.NewRegistry(tools...), def.Deferred)
	req := llm.CompletionRequest{Messages: schemaDiscovery(t, search, "exact", "select:"+base.Name())}
	full := req.Messages[1].Content[0].Content[0].Text
	req.Messages[1].Content[0].Content[0].Text = full[:1000] + "\n... [49000 characters truncated] ...\n" + full[len(full)-100:]
	before, _ := json.Marshal(req)
	out := h.projectToolDocumentation(req)
	h.observeDiscoveredSchemas(out)
	if !h.schemaVisible[base.Name()] {
		t.Fatal("exact truncated definition not restored from registry")
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("audit changed")
	}
	req.Messages[0].Content[0].Input = json.RawMessage(`{"query":"probe"}`)
	h.observeDiscoveredSchemas(h.projectToolDocumentation(req))
	if h.schemaVisible[base.Name()] {
		t.Fatal("broad truncated result authorized")
	}
}
