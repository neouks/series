package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
)

func TestBuiltinPressureEvictsLRUWithoutResurrection(t *testing.T) {
	p, req := builtinFixture(3, nil)
	req.MaxTokens = 256
	for i := 2; i < len(req.Tools); i++ {
		req.Tools[i].Description = strings.Repeat("Complete unique business rule. ", 240)
	}
	p.project(t.Context(), req, false)
	for _, name := range []string{"probe_00", "probe_01", "probe_02"} {
		builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: name})
		p.project(t.Context(), req, false)
	}
	p.used("probe_02")
	projected, err := p.project(t.Context(), req, false)
	if err != nil {
		t.Fatal(err)
	}
	// Optional declarations themselves exceed their budget. History size alone
	// must not repeatedly remove a small reusable working set.
	declarationBytes := 0
	for _, schema := range projected.Tools {
		if !BuiltinResident(p.options.Role, schema.Name) {
			raw, _ := json.Marshal(schema)
			declarationBytes += len(raw)
		}
	}
	p.window = req.MaxTokens + ((declarationBytes+2)/3)*10 - 1
	original, _ := json.Marshal(projected)
	out := p.applyPressure(projected)
	if hasBuiltin(out, "probe_00") || !hasBuiltin(out, "probe_01") || !hasBuiltin(out, "probe_02") || !hasBuiltin(out, "record_fact") {
		t.Fatal("did not evict only least recently used eligible contract")
	}
	after, _ := json.Marshal(projected)
	if string(original) != string(after) {
		t.Fatal("mutated original request")
	}
	p.window = 1000000
	out, _ = p.project(t.Context(), req, false)
	out = p.applyPressure(out)
	if hasBuiltin(out, "probe_00") {
		t.Fatal("lower pressure resurrected an evicted contract")
	}
	h := newDispatchHooks(agentcore.Options{})
	h.builtins = p
	h.observeTools(out.Tools)
	if blocked, why, _ := h.PreToolUse(t.Context(), "probe_00", []byte(`{}`)); !blocked || !strings.Contains(why, "tool_search") {
		t.Fatal("evicted call not guarded", why)
	}
	builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"})
	p.window = 1
	out, _ = p.project(t.Context(), req, false)
	out = p.applyPressure(out)
	if !hasBuiltin(out, "probe_00") || !hasBuiltin(out, "record_fact") {
		t.Fatal("fresh explicit contract/resident removed before it could be used")
	}
	p.options.Mode = "direct"
	out, _ = p.project(t.Context(), req, false)
	out = p.applyPressure(out)
	if len(out.Tools) != len(req.Tools) {
		t.Fatal("direct mode evicted tools")
	}
}

func TestBuiltinPressureSeparatesHistoryAndDeclarationBudgets(t *testing.T) {
	for _, source := range []string{"messages", "system", "output"} {
		t.Run(source, func(t *testing.T) {
			p, req := builtinFixture(1, nil)
			p.window = 12000
			req.MaxTokens = 256
			p.project(t.Context(), req, false)
			builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"})
			for range 12 {
				out, _ := p.project(t.Context(), req, false)
				if !hasBuiltin(p.applyPressure(out), "probe_00") {
					t.Fatal("idle-only eviction")
				}
			}
			switch source {
			case "messages":
				req.Messages = []llm.Message{llm.UserText(strings.Repeat("history", 5000))}
			case "system":
				req.System = []string{strings.Repeat("system", 6000)}
			case "output":
				req.MaxTokens = 11999
			}
			out, _ := p.project(t.Context(), req, false)
			out = p.applyPressure(out)
			wantOptional := source != "output"
			if hasBuiltin(out, "probe_00") != wantOptional || !hasBuiltin(out, "record_fact") {
				t.Fatal("history pressure must preserve definitions; output reserve reduces their budget")
			}
		})
	}
}

func TestBuiltinDeclarationBudgetIsBounded(t *testing.T) {
	for _, test := range []struct{ window, reserve, high, target int }{
		{24000, 4096, 1990, 1592}, {1000000, 4096, 16000, 12800}, {100, 101, 1, 1},
	} {
		high, target := builtinDeclarationBudget(test.window, test.reserve)
		if high != test.high || target != test.target {
			t.Fatal(test, high, target)
		}
	}
}

func TestBuiltinEvictionNoticeFollowsCurrentAuthorization(t *testing.T) {
	p, req := builtinFixture(1, nil)
	p.project(t.Context(), req, false)
	builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"})
	for range 3 {
		p.project(t.Context(), req, false)
	}
	p.window = 1
	out, _ := p.project(t.Context(), req, false)
	projected := out
	raw, _ := json.Marshal(out)
	out = p.applyPressure(out)
	after, _ := json.Marshal(projected)
	if string(raw) != string(after) {
		t.Fatal("notice mutated the original request")
	}
	if hasBuiltin(out, "probe_00") || len(out.Messages) != len(req.Messages)+1 || !strings.Contains(out.Messages[len(out.Messages)-1].Content[0].Text, "probe_00") {
		t.Fatal("no eviction notice")
	}
	original, _ := p.project(t.Context(), req, false)
	p.window = 1000000
	pending := p.applyPressure(original)
	if len(pending.Messages) != len(req.Messages)+1 {
		t.Fatal("pending eviction notice lost")
	}
	withResult := original
	withResult.Messages = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("read", "evidence", false)}}}
	withNotice := p.applyPressure(withResult)
	if len(withNotice.Messages) != 1 || len(withNotice.Messages[0].Content) != 2 || len(withResult.Messages[0].Content) != 1 {
		t.Fatal("notice broke role alternation or mutated tool results")
	}
	builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"})
	out, _ = p.project(t.Context(), req, false)
	out = p.applyPressure(out)
	if !hasBuiltin(out, "probe_00") || len(out.Messages) != len(req.Messages) {
		t.Fatal("reload did not clear notice")
	}
	p.evicted = map[string]bool{"probe_00": true}
	p.options.Check = func(_ context.Context, name string) (string, error) {
		if name == "probe_00" {
			return "unauthorized", nil
		}
		return "", nil
	}
	out, _ = p.project(t.Context(), req, false)
	out = p.applyPressure(out)
	if len(out.Messages) != len(req.Messages) || strings.Contains(out.Tools[0].Description, "probe_00") {
		t.Fatal("revoked identity leaked")
	}
}

func TestBuiltinDeclarationsSurviveLongHistoryAndCompaction(t *testing.T) {
	p, req := builtinFixture(3, nil)
	p.window = 24000
	req.MaxTokens = 4096
	p.project(t.Context(), req, false)
	for _, name := range []string{"probe_00", "probe_01", "probe_02"} {
		builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: name})
	}
	var declarations []llm.ToolSchema
	for i := 0; i < 40; i++ {
		req.Messages = []llm.Message{llm.UserText(strings.Repeat("historical evidence ", i*2000))}
		out, _ := p.project(t.Context(), req, false)
		out = p.applyPressure(out)
		if i == 0 {
			declarations = out.Tools
		}
		if !reflect.DeepEqual(declarations, out.Tools) {
			t.Fatal("history growth changed tool prefix", i)
		}
	}
	req.Messages = []llm.Message{llm.UserText("compacted continuation")}
	out, _ := p.project(t.Context(), req, false)
	out = p.applyPressure(out)
	if !reflect.DeepEqual(declarations, out.Tools) {
		t.Fatal("compaction dropped learned definitions")
	}
}

func TestBuiltinStablePrefixAndAuthorizedDirectory(t *testing.T) {
	p, req := builtinFixture(2, nil)
	first, _ := p.project(t.Context(), req, false)
	builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_01"})
	second, _ := p.project(t.Context(), req, false)
	builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"})
	third, _ := p.project(t.Context(), req, false)
	if !reflect.DeepEqual(first.Tools, second.Tools[:len(first.Tools)]) || !reflect.DeepEqual(second.Tools, third.Tools[:len(second.Tools)]) {
		t.Fatal("loading changed a cached declaration prefix")
	}
	if !strings.Contains(first.Tools[0].Description, "probe_00") || strings.Contains(first.Tools[0].Description, "properties") {
		t.Fatal("discovery index missing or leaked contracts")
	}
	p.options.Check = func(_ context.Context, name string) (string, error) {
		if name == "probe_00" {
			return "unauthorized", nil
		}
		return "", nil
	}
	out, err := p.project(t.Context(), req, false)
	if err != nil || hasBuiltin(out, "probe_00") || strings.Contains(out.Tools[0].Description, "probe_00") {
		t.Fatal("revoked name remains discoverable")
	}
}

func TestBuiltinDirectoryRankingAndFallbackCursor(t *testing.T) {
	if builtinKeywordScore("approved 审批资产列表 hostname", "list_task_assets", "只读审批管理视图，资产列表") <= builtinKeywordScore("approved 审批资产列表 hostname", "asset_get", "读取资产详情") {
		t.Fatal("irrelevant words defeated partial relevance")
	}
	if builtinKeywordScore("asset_get", "asset_get", "读取详情") <= builtinKeywordScore("asset_get", "asset_search", "详情用asset_get") {
		t.Fatal("name not ranked first")
	}
	p, req := builtinFixture(20, nil)
	p.project(t.Context(), req, false)
	fallback := builtinDiscover(t, p, builtinDiscoveryInput{Mode: "keyword", Query: "no lexical matches"})
	next := *fallback.Next
	next.Mode = "catalog"
	next.Query = ""
	page := builtinDiscover(t, p, next)
	if page.Status == "catalog_changed" || len(page.Candidates) == 0 || len(p.active) != 0 {
		t.Fatal("same fallback catalog view lost its cursor", page)
	}
}

func TestBuiltinCatalogHistoryPressureProjection(t *testing.T) {
	receipt := builtinDiscoveryResult{Version: 1, Status: "loaded", Loaded: []string{"probe_00"}, Candidates: []builtinCandidate{{Name: "probe_00", Summary: strings.Repeat("summary ", 800)}}}
	raw, _ := json.Marshal(receipt)
	req := llm.CompletionRequest{Messages: []llm.Message{documentationCall("s", BuiltinSearchName, `{"query":"probe","mode":"keyword"}`), {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("s", string(raw), false)}}, {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("continue")}}}}
	before, _ := json.Marshal(req)
	out := compactBuiltinCatalogHistory(req)
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("raw history changed")
	}
	text := out.Messages[1].Content[0].Content[0].Text
	if strings.Contains(text, "summary summary") || !strings.Contains(text, `"loaded":["probe_00"]`) {
		t.Fatal("did not retain receipt while removing directory prose")
	}
	fresh := req
	fresh.Messages = fresh.Messages[:2]
	if !reflect.DeepEqual(fresh, compactBuiltinCatalogHistory(fresh)) {
		t.Fatal("fresh result removed")
	}
}
