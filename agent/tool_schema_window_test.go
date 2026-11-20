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
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func schemaWindowTools(count, padding int, run func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error)) []actool.CoreTool {
	var tools []actool.CoreTool
	for i := 0; i < count; i++ {
		schema := map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{
			"type": "string", "enum": []string{"safe"}, "description": "PARAMETER_DESCRIPTION" + strings.Repeat("x", padding),
		}}, "required": []string{"mode"}, "additionalProperties": false}
		tools = append(tools, actool.Build(actool.Spec{Name: fmt.Sprintf("probe_%03d", i), Description: "按需探测", Schema: schema, Run: run}))
	}
	return tools
}

func schemaDiscovery(t *testing.T, search actool.CoreTool, id, query string) []llm.Message {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"query": query})
	result, err := search.Call(t.Context(), input, nil)
	if err != nil || result.IsError {
		t.Fatalf("discovery: %v", err)
	}
	return []llm.Message{documentationCall(id, actool.SearchExtraToolsName, string(input)),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, result.Flatten(), false)}}}
}

func schemaWindowContents(t *testing.T, req llm.CompletionRequest) map[string]discoveredContract {
	t.Helper()
	out := map[string]discoveredContract{}
	for _, result := range discoveryHistory(req) {
		contracts, _ := retainedContracts(result.text)
		for _, contract := range contracts {
			if _, duplicate := out[contract.Name]; duplicate {
				t.Fatalf("duplicate schema for %s", contract.Name)
			}
			out[contract.Name] = contract
		}
	}
	return out
}

func TestToolSchemaWindowPressureInsteadOfCount(t *testing.T) {
	tools := schemaWindowTools(100, 6000, nil)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	search := actool.NewSearchExtraTools(actool.NewRegistry(tools...), names)
	req := llm.CompletionRequest{}
	for i, name := range names {
		req.Messages = append(req.Messages, schemaDiscovery(t, search, fmt.Sprint(i), "select:"+name)...)
	}
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("next")}})
	before, _ := json.Marshal(req)
	spacious := (&toolSchemaPressure{window: 1000000}).projectDocumentation(req)
	if got := len(schemaWindowContents(t, spacious)); got != 100 {
		t.Fatalf("spacious request lost contracts: %d", got)
	}
	p := &toolSchemaPressure{window: 64000}
	out := p.projectDocumentation(req)
	contracts := schemaWindowContents(t, out)
	if len(contracts) == 0 || len(contracts) >= 100 || requestInputEstimate(out) > 64000*65/100-8192 {
		t.Fatalf("pressure not relieved: %d / %d", len(contracts), requestInputEstimate(out))
	}
	if _, ok := contracts[names[0]]; ok {
		t.Fatal("oldest retained")
	}
	if _, ok := contracts[names[99]]; !ok {
		t.Fatal("newest lost")
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) || !reflect.DeepEqual(out, p.projectDocumentation(req)) || !reflect.DeepEqual(out, p.projectDocumentation(out)) {
		t.Fatal("audit changed or unstable projection")
	}
	// Increasing capacity does not resurrect previously evicted contracts.
	p.window = 1000000
	if got := len(schemaWindowContents(t, p.projectDocumentation(req))); got != len(contracts) {
		t.Fatal("evicted history resurrected")
	}
	req.Messages = append(req.Messages, schemaDiscovery(t, search, "rediscovery", "select:"+names[0])...)
	if _, ok := schemaWindowContents(t, p.projectDocumentation(req))[names[0]]; !ok {
		t.Fatal("explicit rediscovery did not restore")
	}
}

func TestToolSchemaWindowWholeRequestAndFreshOversize(t *testing.T) {
	tools := schemaWindowTools(12, 0, nil)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	search := actool.NewSearchExtraTools(actool.NewRegistry(tools...), names)
	req := llm.CompletionRequest{Messages: schemaDiscovery(t, search, "batch", "probe")}
	if got := len(schemaWindowContents(t, projectToolDocumentation(req))); got != 12 {
		t.Fatalf("count triggered eviction: %d", got)
	}
	req.System = []string{strings.Repeat("ordinary context ", 3000)}
	if got := len(schemaWindowContents(t, (&toolSchemaPressure{window: 10000}).projectDocumentation(req))); got != 0 {
		t.Fatal("system pressure ignored")
	}
	large := schemaWindowTools(1, 40000, nil)[0]
	search = actool.NewSearchExtraTools(actool.NewRegistry(large), []string{large.Name()})
	for _, exact := range []bool{false, true} {
		query := "probe"
		if exact {
			query = "select:" + large.Name()
		}
		req = llm.CompletionRequest{Messages: schemaDiscovery(t, search, "large", query)}
		p := &toolSchemaPressure{window: 10000}
		if got := len(schemaWindowContents(t, p.projectDocumentation(req))); (got == 1) != exact {
			t.Fatalf("fresh exact %v retained %d", exact, got)
		}
		req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("next")}})
		if len(schemaWindowContents(t, p.projectDocumentation(req))) != 0 {
			t.Fatal("oversized schema survived pressure")
		}
	}
}

func TestToolSchemaWindowKeepsLatestDefinitionAcrossQueries(t *testing.T) {
	old := schemaWindowTools(1, 0, nil)[0]
	current := schemaWindowTools(1, 10, nil)[0]
	search := actool.NewSearchExtraTools(actool.NewRegistry(old), []string{old.Name()})
	req := llm.CompletionRequest{Messages: schemaDiscovery(t, search, "keyword", "probe")}
	req.Messages = append(req.Messages, schemaDiscovery(t, search, "exact", "select:"+old.Name())...)
	search = actool.NewSearchExtraTools(actool.NewRegistry(current), []string{current.Name()})
	req.Messages = append(req.Messages, schemaDiscovery(t, search, "changed", "select:"+current.Name())...)
	out := projectToolDocumentation(req)
	contracts := schemaWindowContents(t, out)
	if len(contracts) != 1 || !matchingSchema(contracts[current.Name()].Schema, current.InputSchema()) {
		t.Fatal("did not retain just the newest complete definition")
	}
	if !reflect.DeepEqual(out, projectToolDocumentation(out)) {
		t.Fatal("repeat projection changed the selected version")
	}
}

func TestToolSchemaWindowGateUsesCurrentRequestAndDefinition(t *testing.T) {
	base := schemaWindowTools(1, 0, nil)[0]
	tools, def := OnDemandTools([]actool.CoreTool{base}, DeferredInfo{}, "worker")
	// Wrappers applied after catalog assembly must retain the gate too.
	tools[0] = &interactiveTool{CoreTool: DecorateTool(guardPanic(tools[0]), "wrapped", tools[0].InputSchema())}
	opts := agentcore.Options{Tools: tools, DeferredTools: def.Deferred, UnlockSet: def.Unlock, PermissionMode: permission.ModeBypass}
	search := actool.NewSearchExtraTools(actool.NewRegistry(tools...), def.Deferred)
	req := llm.CompletionRequest{Messages: schemaDiscovery(t, search, "s", "select:"+base.Name())}
	invoke := []byte(`{"tool_name":"probe_000","params":{"mode":"safe"}}`)
	h := newDispatchHooks(opts)
	if blocked, _, _ := h.PreToolUse(t.Context(), actool.ExecuteExtraToolName, invoke); !blocked {
		t.Fatal("undiscovered call allowed")
	}
	h.observeDiscoveredSchemas(projectToolDocumentation(req))
	if blocked, reason, _ := h.PreToolUse(t.Context(), actool.ExecuteExtraToolName, invoke); blocked {
		t.Fatalf("restored complete schema blocked: %s", reason)
	}
	// Context compaction removes the result. Prior discovery is not a lease.
	h.observeDiscoveredSchemas(llm.CompletionRequest{})
	if blocked, _, _ := h.PreToolUse(t.Context(), actool.ExecuteExtraToolName, invoke); !blocked {
		t.Fatal("compacted schema still authorized")
	}
	for _, variant := range []string{"stale", "guidance", "failed", "prose", "unpaired"} {
		t.Run(variant, func(t *testing.T) {
			test := cloneToolMessages(req)
			switch variant {
			case "guidance":
				test.Messages[1].Content[0].Content = []llm.ContentBlock{llm.TextBlock(strings.Replace(test.Messages[1].Content[0].Content[0].Text, "wrapped", "changed semantics", 1))}
			case "stale":
				test.Messages[1].Content[0].Content = []llm.ContentBlock{llm.TextBlock(strings.Replace(test.Messages[1].Content[0].Content[0].Text, `"safe"`, `"old"`, 1))}
			case "failed":
				test.Messages[1].Content[0].IsError = true
			case "prose":
				test.Messages = []llm.Message{llm.UserText(req.Messages[1].Content[0].Content[0].Text)}
			case "unpaired":
				test.Messages = test.Messages[1:]
			}
			h.observeDiscoveredSchemas(projectToolDocumentation(test))
			if blocked, _, _ := h.PreToolUse(t.Context(), actool.ExecuteExtraToolName, invoke); !blocked {
				t.Fatal("invalid discovery lease")
			}
		})
	}
	h.observeDiscoveredSchemas(projectToolDocumentation(req))
	h.unlock = actool.NewUnlockSet()
	if blocked, _, _ := h.PreToolUse(t.Context(), actool.ExecuteExtraToolName, invoke); !blocked {
		t.Fatal("schema bypassed unlock gate")
	}
}

func TestToolSchemaWindowSessionEvictionAndRediscovery(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, hosted := range []bool{false, true} {
			for _, noa := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v/hosted=%v/noa=%v", stream, hosted, noa), func(t *testing.T) {
					calls, turn := 0, 0
					base := schemaWindowTools(9, 6000, func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
						calls++
						return actool.Text("executed"), nil
					})
					tools, def := OnDemandTools(base, DeferredInfo{}, "worker")
					system, boundary := deferredSystem("test", def)
					invoke := `{"tool_name":"probe_000","params":{"mode":"safe"}}`
					complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
						if turn == 2 && calls != 0 {
							t.Error("same-response discovery allowed guessed execution")
						}
						if turn >= 4 && turn <= 6 {
							if _, found := schemaWindowContents(t, req)["probe_000"]; found {
								t.Error("evicted schema still present")
							}
						}
						if turn == 7 && calls != 1 {
							t.Error("evicted tool executed before rediscovery")
						}
						var response llm.Message
						switch turn {
						case 0, 2, 4, 7:
							response = documentationCall(fmt.Sprint(turn), actool.ExecuteExtraToolName, invoke)
						case 1:
							response = documentationCall("search", actool.SearchExtraToolsName, `{"query":"select:probe_000"}`)
							response.Content = append(response.Content, documentationCall("guess", actool.ExecuteExtraToolName, invoke).Content...)
						case 3:
							response = documentationCall("fill", actool.SearchExtraToolsName, `{"query":"select:probe_001,probe_002,probe_003,probe_004,probe_005,probe_006,probe_007,probe_008"}`)
						case 5:
							response = documentationCall("direct-guess", "probe_000", `{"mode":"safe"}`)
						case 6:
							response = documentationCall("reload", actool.SearchExtraToolsName, `{"query":"select:probe_000"}`)
						default:
							return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
						}
						turn++
						return response, "tool_use", llm.Usage{}, nil
					}
					opts := agentcore.Options{Tools: tools, DeferredTools: def.Deferred, UnlockSet: def.Unlock, SystemPrompt: system, DynamicBoundary: boundary,
						DisableBackgroundTasks: true, NonStreaming: !stream, PermissionMode: permission.ModeBypass, Deps: sdkTurnDeps(complete)}
					if hosted {
						opts.Transcript = transcript.NewStore(t.TempDir())
					}
					if noa {
						enableNoa(&opts, func() bool { return true }, t.TempDir(), "schemas", func(s string) { t.Error(s) })
					}
					if opts.Compactor == nil {
						opts.Compactor = pressureTestCompactor{}
					}
					opts.Compactor = &contextWindowCompactor{Compactor: opts.Compactor, window: 12000}
					if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
						t.Fatal(err)
					}
					if calls != 2 || turn != 8 {
						t.Fatalf("calls=%d turn=%d", calls, turn)
					}
				})
			}
		}
	}
}

// Disable unrelated SDK summarization to exercise projection/lease behavior.
type pressureTestCompactor struct{}

func (pressureTestCompactor) Pre(_ context.Context, m []llm.Message, _ int) []llm.Message { return m }
func (pressureTestCompactor) Reactive(_ context.Context, m []llm.Message) ([]llm.Message, bool) {
	return m, false
}
func (pressureTestCompactor) IsOverflow(error) bool { return false }
