package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func builtinFixture(count int, check func(context.Context, string) (string, error)) (*builtinCatalog, llm.CompletionRequest) {
	p := (&builtinCatalog{options: BuiltinLoadingOptions{Role: "worker", Mode: "on_demand", SearchAllowed: true, Known: map[string]bool{BuiltinSearchName: true, "record_fact": true}, Check: check}}).fresh()
	req := llm.CompletionRequest{Tools: []llm.ToolSchema{{Name: BuiltinSearchName, InputSchema: obj(nil)}, {Name: "record_fact", InputSchema: obj(nil)}}}
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("probe_%02d", i)
		p.options.Known[name] = true
		req.Tools = append(req.Tools, llm.ToolSchema{Name: name, Description: "isolated capability UNIQUE_FULL_GUIDANCE; mode=safe only", InputSchema: obj(map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"safe"}}})})
	}
	return p, req
}
func hasBuiltin(req llm.CompletionRequest, name string) bool {
	for _, t := range req.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
}
func builtinDiscover(t *testing.T, p *builtinCatalog, in builtinDiscoveryInput) builtinDiscoveryResult {
	t.Helper()
	if in.Limit == 0 {
		in.Limit = 1
	}
	r, err := p.discover(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuiltinDiscoveryModesAuthorizationAndPagination(t *testing.T) {
	denied := false
	p, req := builtinFixture(21, func(_ context.Context, name string) (string, error) {
		if denied && name == "probe_00" {
			return "unauthorized", nil
		}
		return "", nil
	})
	if _, err := p.project(t.Context(), req, false); err != nil {
		t.Fatal(err)
	}
	page := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "catalog"})
	if page.Status != "candidates" || len(page.Candidates) != 8 || !page.HasMore || len(p.active) != 0 {
		t.Fatalf("catalog activated tools: %+v", page)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), "params_schema") || strings.Contains(string(raw), "properties") {
		t.Fatal("catalog carries schema")
	}
	found := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "keyword", Query: "isolated"})
	if len(found.Loaded) != 1 || found.Loaded[0] != "probe_00" {
		t.Fatalf("default loaded too much: %+v", found)
	}
	if p.visible["probe_00"] {
		t.Fatal("search changed current declaration")
	}
	out, _ := p.project(t.Context(), req, false)
	if !hasBuiltin(out, "probe_00") || hasBuiltin(out, "probe_01") {
		t.Fatal("incorrect next-request load")
	}
	if got := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "auto", Query: "probe_00"}); got.Status != "already_loaded" {
		t.Fatal(got)
	}
	if got := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "keyword", Query: "isolated"}); !reflect.DeepEqual(got.Loaded, []string{"probe_01"}) {
		t.Fatalf("keyword did not prefer an undeclared match: %+v", got)
	}
	if got := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "keyword", Query: "isolated", Limit: 20}); len(got.Loaded)+len(got.AlreadyLoaded) != 20 {
		t.Fatalf("explicit load limit not honored: %+v", got)
	}
	if got := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "keyword", Query: "unmatched synonym"}); got.Fallback != "catalog" || len(got.Candidates) == 0 || len(got.Loaded) != 0 {
		t.Fatal(got)
	}
	denied = true
	if got := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"}); got.Status != "unauthorized" {
		t.Fatal(got)
	}
	if got := builtinDiscover(t, p, *page.Next); got.Status != "catalog_changed" || got.Next.Offset != 0 {
		t.Fatal(got)
	}
	p.options.Known["offline"] = true
	if got := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "offline"}); got.Status != "unavailable" {
		t.Fatal(got)
	}
	if got := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "absent"}); got.Status != "not_found" {
		t.Fatal(got)
	}
	for _, in := range []builtinDiscoveryInput{{Mode: "keyword", Query: "probe", Limit: 21}, {Mode: "exact", Query: "probe", Limit: 1, Offset: -1}, {Mode: "bad", Query: "probe", Limit: 1}} {
		if _, err := p.discover(t.Context(), in); err == nil {
			t.Fatal("invalid discovery input")
		}
	}
}

func TestBuiltinRestoreRejectsInvalidReceipts(t *testing.T) {
	call := documentationCall("s", BuiltinSearchName, `{"query":"probe_00","mode":"exact"}`)
	result := llm.ToolResultText("s", `{"discovery_version":1,"status":"loaded","loaded":["probe_00"]}`, false)
	failed := result
	failed.IsError = true
	duplicateCall := call
	duplicateCall.Content = append(append([]llm.ContentBlock(nil), call.Content...), call.Content...)
	for name, history := range map[string][]llm.Message{
		"unpaired":           {{Role: llm.RoleUser, Content: []llm.ContentBlock{result}}},
		"wrong_role":         {call, {Role: "system", Content: []llm.ContentBlock{result}}},
		"failed":             {call, {Role: llm.RoleUser, Content: []llm.ContentBlock{failed}}},
		"user_text_boundary": {call, llm.UserText("unrelated"), {Role: llm.RoleUser, Content: []llm.ContentBlock{result}}},
		"duplicate_result":   {call, {Role: llm.RoleUser, Content: []llm.ContentBlock{result, result}}},
		"duplicate_call":     {duplicateCall, {Role: llm.RoleUser, Content: []llm.ContentBlock{result}}},
		"malformed":          {call, {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s", "loaded probe_00", false)}}},
		"catalog":            {call, {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s", `{"discovery_version":1,"status":"candidates","loaded":["probe_00"]}`, false)}}},
	} {
		t.Run(name, func(t *testing.T) {
			p, req := builtinFixture(1, nil)
			req.Messages = history
			out, err := p.project(t.Context(), req, false)
			if err != nil || hasBuiltin(out, "probe_00") {
				t.Fatalf("invalid receipt restored tool: %v", err)
			}
		})
	}
	prototype, req := builtinFixture(1, nil)
	a, b := prototype.fresh(), prototype.fresh()
	a.project(t.Context(), req, false)
	builtinDiscover(t, a, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"})
	out, err := b.project(t.Context(), req, false)
	if err != nil || hasBuiltin(out, "probe_00") {
		t.Fatal("discovery leaked across sessions")
	}
}

func TestBuiltinDeclarationLifetimeResumeAndDirect(t *testing.T) {
	p, req := builtinFixture(2, nil)
	p.project(t.Context(), req, false)
	receipt := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"})
	before, _ := json.Marshal(req)
	for i := 0; i < 3; i++ {
		out, err := p.project(t.Context(), req, false)
		if err != nil {
			t.Fatal(err)
		}
		if !hasBuiltin(out, "probe_00") {
			t.Fatalf("idle request %d", i)
		}
		if !hasBuiltin(out, "record_fact") {
			t.Fatal("resident expired")
		}
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("mutated caller")
	}
	builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"})
	for i := 0; i < 5; i++ {
		out, _ := p.project(t.Context(), req, false)
		if !hasBuiltin(out, "probe_00") {
			t.Fatal("continuous use expired")
		}
		p.used("probe_00")
	}
	raw, _ := json.Marshal(receipt)
	history := []llm.Message{documentationCall("s", BuiltinSearchName, `{"query":"probe_00","mode":"exact"}`), {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s", string(raw), false)}}}
	for _, age := range []int{0, 1, 2, 4} {
		resumed, _ := builtinFixture(2, nil)
		r := req
		r.Messages = append([]llm.Message(nil), history...)
		for i := 0; i < age; i++ {
			r.Messages = append(r.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("idle")}}, llm.UserText("continue"))
		}
		out, _ := resumed.project(t.Context(), r, false)
		if !hasBuiltin(out, "probe_00") {
			t.Fatalf("resume age %d", age)
		}
	}
	p.options.Mode = "direct"
	for i := 0; i < 5; i++ {
		out, _ := p.project(t.Context(), req, false)
		if !hasBuiltin(out, "probe_01") {
			t.Fatal("direct expired")
		}
	}
}

func TestBuiltinNativeSessionLifecycle(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, hosted := range []bool{false, true} {
			for _, noa := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v/hosted=%v/noa=%v", stream, hosted, noa), func(t *testing.T) {
					calls, turn := 0, 0
					revoked := false
					base := actool.Build(actool.Spec{Name: "http_request", Description: "Request contract mode=safe", Schema: map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"safe"}}}, "required": []string{"mode"}}, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
						calls++
						return actool.Text("ok"), nil
					}})
					tools := ConfigureBuiltinTools([]actool.CoreTool{base}, BuiltinLoadingOptions{Role: "worker", SearchAllowed: true, Check: func(_ context.Context, name string) (string, error) {
						if revoked && name == "http_request" {
							return "unauthorized", nil
						}
						return "", nil
					}})
					complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
						active := hasBuiltin(req, "http_request")
						if active != (turn >= 2 && turn <= 7) {
							t.Errorf("turn %d active=%v", turn, active)
						}
						switch turn {
						case 0, 2, 5, 7, 8:
							if turn == 7 {
								revoked = true
							}
							turn++
							return documentationCall(fmt.Sprint(turn), "http_request", `{"mode":"safe"}`), "tool_use", llm.Usage{}, nil
						case 1, 6:
							turn++
							m := documentationCall(fmt.Sprint(turn), BuiltinSearchName, `{"mode":"exact","query":"http_request"}`)
							if turn == 2 {
								m.Content = append(m.Content, documentationCall("guess", "http_request", `{"mode":"safe"}`).Content...)
							}
							return m, "tool_use", llm.Usage{}, nil
						case 3, 4:
							turn++
							return documentationCall(fmt.Sprint(turn), BuiltinSearchName, `{"mode":"catalog","query":""}`), "tool_use", llm.Usage{}, nil
						default:
							return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
						}
					}
					opts := agentcore.Options{Tools: tools, NonStreaming: !stream, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, Deps: sdkTurnDeps(complete)}
					if hosted {
						opts.Transcript = transcript.NewStore(t.TempDir())
					}
					if noa {
						enableNoa(&opts, func() bool { return true }, t.TempDir(), "builtin", func(s string) { t.Error(s) })
					}
					if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
						t.Fatal(err)
					}
					if calls != 2 || turn != 9 {
						t.Fatalf("calls=%d turn=%d", calls, turn)
					}
				})
			}
		}
	}
}

func TestBuiltinPlannerCeilingAndSettlement(t *testing.T) {
	tools := ConfigureBuiltinTools([]actool.CoreTool{actool.NewBash(), actool.Build(actool.Spec{Name: "http_request"}), actool.Build(actool.Spec{Name: "add_intent", Schema: obj(nil)})}, BuiltinLoadingOptions{Role: "planner", SearchAllowed: true, Mode: "direct"})
	for _, tool := range tools {
		if tool.Name() == "Bash" || tool.Name() == "http_request" {
			t.Fatal("planner ceiling bypass")
		}
	}
	p, req := builtinFixture(2, nil)
	p.project(t.Context(), req, false)
	p.options.Known["report_finding"] = true
	req.Tools = append(req.Tools, llm.ToolSchema{Name: "report_finding", InputSchema: obj(nil)})
	out, _ := p.project(t.Context(), req, true)
	if !hasBuiltin(out, "report_finding") || hasBuiltin(out, "probe_01") {
		t.Fatal("settlement writeback not retained")
	}
	req.Tools = req.Tools[:2]
	out, _ = p.project(t.Context(), req, true)
	if hasBuiltin(out, "report_finding") {
		t.Fatal("settlement reintroduced disabled tool")
	}
	// A tool disabled only during settlement is still discoverable beforehand.
	tools = ConfigureBuiltinTools([]actool.CoreTool{actool.Build(actool.Spec{Name: "http_request", Schema: obj(nil)})}, BuiltinLoadingOptions{Role: "worker", SearchAllowed: true})
	opts := agentcore.Options{Tools: tools, Settlement: &harness.Settlement{DisabledTools: []string{"http_request"}}}
	configureDeferredSettlement(&opts)
	h := newDispatchHooks(opts)
	h.observeTools([]llm.ToolSchema{{Name: BuiltinSearchName}, {Name: settlementMarkerName}})
	if blocked, reason, _ := h.PreToolUse(t.Context(), "http_request", []byte(`{}`)); !blocked || !strings.Contains(reason, "tool_search") {
		t.Fatalf("undiscovered tool got wrong recovery guidance: %s", reason)
	}
	h.observeTools([]llm.ToolSchema{{Name: BuiltinSearchName}})
	if blocked, reason, _ := h.PreToolUse(t.Context(), "http_request", []byte(`{}`)); !blocked || strings.Contains(reason, "tool_search") {
		t.Fatalf("settlement block incorrectly advised rediscovery: %s", reason)
	}
}

func TestBuiltinLegacyDefinitionsRemovedWithoutMutatingHistory(t *testing.T) {
	base, full := documentationFixture(t)
	p, req := builtinFixture(0, nil)
	p.options.Known[base.Name()] = true
	req.Messages = []llm.Message{documentationCall("s", actool.SearchExtraToolsName, `{"query":"select:custom_probe"}`), {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s", full, false)}}}
	before, _ := json.Marshal(req)
	out, _ := p.project(t.Context(), req, false)
	if strings.Contains(out.Messages[1].Content[0].Content[0].Text, "params schema") {
		t.Fatal("old native schema survived")
	}
	after, _ := json.Marshal(req)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("raw history changed")
	}
}

func TestBuiltinSettlementNativeSession(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			called, turn := 0, 0
			report := actool.Build(actool.Spec{Name: "report_finding", Schema: obj(nil), Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				called++
				return actool.Text("saved"), nil
			}})
			bash := actool.Build(actool.Spec{Name: "Bash", Schema: obj(nil), Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				t.Error("settlement executed Bash")
				return actool.Text("bad"), nil
			}})
			tools := ConfigureBuiltinTools([]actool.CoreTool{report, bash}, BuiltinLoadingOptions{Role: "mainagent", SearchAllowed: true})
			complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				if turn == 0 {
					if hasBuiltin(req, "report_finding") {
						t.Error("writeback loaded before settlement")
					}
					turn++
					return documentationCall("cat", BuiltinSearchName, `{"mode":"catalog","query":""}`), "tool_use", llm.Usage{}, nil
				}
				if hasBuiltin(req, "Bash") || !hasBuiltin(req, "report_finding") || hasBuiltin(req, settlementMarkerName) {
					t.Error("wrong settlement declarations")
				}
				if turn == 1 {
					turn++
					m := documentationCall("blocked", "Bash", `{}`)
					m.Content = append(m.Content, documentationCall("save", "report_finding", `{}`).Content...)
					return m, "tool_use", llm.Usage{}, nil
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
			}
			opts := agentcore.Options{Tools: tools, NonStreaming: !stream, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, MaxTurns: 1, Settlement: &harness.Settlement{Prompt: "write back", DisabledTools: []string{"Bash"}, MaxTurns: 3}, Deps: sdkTurnDeps(complete)}
			if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
				t.Fatal(err)
			}
			if called != 1 {
				t.Fatal("writeback failed")
			}
		})
	}
}
