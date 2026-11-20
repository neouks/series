package agent

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
)

// Resident and host tools form a stable prefix. Newly discovered definitions
// append after existing ones instead of inserting before a cached prefix.
func (p *builtinCatalog) orderDeclarations(tools []llm.ToolSchema) []llm.ToolSchema {
	stable := make([]llm.ToolSchema, 0, len(tools))
	dynamic := map[string]llm.ToolSchema{}
	for _, t := range tools {
		if p.options.Mode == "direct" || !p.options.Known[t.Name] || BuiltinResident(p.options.Role, t.Name) {
			stable = append(stable, t)
		} else {
			dynamic[t.Name] = t
			if !slices.Contains(p.declarationOrder, t.Name) {
				p.declarationOrder = append(p.declarationOrder, t.Name)
			}
		}
	}
	order := make([]string, 0, len(dynamic))
	for _, name := range p.declarationOrder {
		if t, ok := dynamic[name]; ok {
			stable = append(stable, t)
			order = append(order, name)
		}
	}
	p.declarationOrder = order
	return stable
}

// The small discovery index lists ONLY authorized, assembled nonresident names.
// It is independent of activation, contains no contracts and confers no grants.
func (p *builtinCatalog) describeDirectory(req llm.CompletionRequest) llm.CompletionRequest {
	if p.options.Mode == "direct" {
		return req
	}
	names := []string{}
	for name := range p.eligible {
		if name != BuiltinSearchName && !BuiltinResident(p.options.Role, name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	lines := []string{}
	for _, name := range names[:min(len(names), 24)] {
		lines = append(lines, name+": "+compactToolText(p.schemas[name].Description, 36))
	}
	if len(names) > 24 {
		lines = append(lines, "更多获授权工具用 catalog 分页查看。")
	}
	for i, t := range req.Tools {
		if t.Name == BuiltinSearchName && len(lines) > 0 {
			req.Tools = append([]llm.ToolSchema(nil), req.Tools...)
			req.Tools[i].Description = t.Description + "\n其他获授权内置能力（已声明的可直接用；未声明用 exact 加载）：\n" + strings.Join(lines, "\n")
			break
		}
	}
	return req
}

// Tool declarations get their own bounded budget. Growing evidence/history is
// handled by the existing context manager, not repeated tool unload/reload.
// The caps are ARTEX compatibility policy, not claimed OpenAI implementation.
func builtinDeclarationBudget(window, reserve int) (high, target int) {
	available := max(1, window-reserve)
	high = max(1, min(16000, available/10))
	target = max(1, high*4/5)
	return
}

// Run AFTER external-history projection. Bytes/3 is a conservative estimate,
// never provider usage. Full request pressure can trim old catalog prose; only
// optional declaration pressure removes definitions, keeping small working sets
// stable even when the history alone is larger than the context budget.
func (p *builtinCatalog) applyPressure(req llm.CompletionRequest) llm.CompletionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.options.Mode == "direct" {
		return req
	}
	window := p.window
	if window <= 0 {
		window = defaultWindowK * 1000
	}
	reserve := req.MaxTokens
	if reserve <= 0 {
		reserve = min(8192, window/5)
	}
	contextHigh := max(1, window*80/100-reserve)
	estimate := func(r llm.CompletionRequest) int { return requestInputEstimate(withoutSettlementMarker(r)) }
	if estimate(req) > contextHigh {
		req = compactBuiltinCatalogHistory(req)
	}
	high, target := builtinDeclarationBudget(window, reserve)
	type candidate struct {
		name       string
		last, size int
	}
	candidates := []candidate{}
	declarationBytes := 0
	for _, t := range req.Tools {
		last, loaded := p.active[t.Name]
		if !p.options.Known[t.Name] || BuiltinResident(p.options.Role, t.Name) {
			continue
		}
		raw, _ := json.Marshal(t)
		declarationBytes += len(raw)
		if !loaded || (p.settling && builtinSettlementTool(t.Name)) {
			continue
		}
		// A discovery or successful invocation gets its immediate next response.
		if p.request-last <= 1 {
			continue
		}
		candidates = append(candidates, candidate{t.Name, last, len(raw)})
	}
	if (declarationBytes+2)/3 <= high {
		return p.evictionNotice(req)
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.last != b.last {
			return a.last < b.last
		}
		if a.size != b.size {
			return a.size > b.size
		}
		return a.name < b.name
	})
	for _, c := range candidates {
		out := make([]llm.ToolSchema, 0, len(req.Tools)-1)
		for _, t := range req.Tools {
			if t.Name != c.name {
				out = append(out, t)
			}
		}
		req.Tools = out
		delete(p.active, c.name)
		delete(p.visible, c.name)
		if p.evicted == nil {
			p.evicted = map[string]bool{}
		}
		p.evicted[c.name] = true
		p.declarationOrder = slices.DeleteFunc(p.declarationOrder, func(n string) bool { return n == c.name })
		declarationBytes -= c.size
		if (declarationBytes+2)/3 <= target {
			break
		}
	}
	return p.evictionNotice(req)
}

// Caller holds p.mu. Put the small status at the request tail, never in the
// stable tool description/system prefix, and never mutate the stored transcript.
func (p *builtinCatalog) evictionNotice(req llm.CompletionRequest) llm.CompletionRequest {
	names := []string{}
	for name := range p.evicted {
		if p.visible[name] || !p.eligible[name] {
			delete(p.evicted, name)
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return req
	}
	sort.Strings(names)
	raw, _ := json.Marshal(names[:min(8, len(names))])
	note := "<system-reminder>内置工具声明更新：以下工具因按需定义预算退出本轮声明：" + string(raw) + "。如需使用，先用 tool_search exact 重新加载，下一轮查看完整参数再调用；不要照抄历史调用。其他未声明工具同样须先发现。</system-reminder>"
	req.Messages = append([]llm.Message(nil), req.Messages...)
	last := len(req.Messages) - 1
	if last >= 0 && req.Messages[last].Role == llm.RoleUser {
		req.Messages[last].Content = append(append([]llm.ContentBlock(nil), req.Messages[last].Content...), llm.TextBlock(note))
	} else {
		req.Messages = append(req.Messages, llm.UserText(note))
	}
	return req
}

// Only old paired successful discovery responses are projected. Keep loading
// receipts for resume; remove candidate prose under pressure, never raw audit.
func compactBuiltinCatalogHistory(req llm.CompletionRequest) llm.CompletionRequest {
	lastAssistant := -1
	for i, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			lastAssistant = i
		}
	}
	pending := map[string]bool{}
	seen := map[string]bool{}
	cloned := false
	for mi, m := range req.Messages {
		if m.Role == llm.RoleAssistant || m.Role == "system" {
			clear(pending)
			clear(seen)
		}
		for bi, b := range m.Content {
			if b.Type == llm.BlockToolUse && m.Role == llm.RoleAssistant {
				delete(pending, b.ID)
				if b.ID != "" && !seen[b.ID] && b.Name == BuiltinSearchName {
					pending[b.ID] = true
				}
				seen[b.ID] = true
				continue
			}
			if b.Type != llm.BlockToolResult {
				if m.Role == llm.RoleUser {
					clear(pending)
				}
				continue
			}
			paired := pending[b.ToolUseID]
			delete(pending, b.ToolUseID)
			if !paired || m.Role != llm.RoleUser || mi >= lastAssistant || b.IsError || len(b.Content) != 1 || b.Content[0].Type != llm.BlockText {
				continue
			}
			text := noaadapter.StripRefTag(b.Content[0].Text)
			var result builtinDiscoveryResult
			if json.Unmarshal([]byte(text), &result) != nil || result.Version != 1 || len(result.Candidates) == 0 {
				continue
			}
			value := map[string]any{"discovery_version": 1, "status": result.Status, "loaded": result.Loaded, "already_loaded": result.AlreadyLoaded, "catalog_candidates_omitted": true, "message": "目录候选已因上下文压力省略；当前参数以本轮工具声明为准，未声明的工具用 tool_search 精确加载。"}
			raw, _ := json.Marshal(value)
			if len(raw) >= len(text) {
				continue
			}
			if !cloned {
				req = cloneToolMessages(req)
				cloned = true
			}
			block := &req.Messages[mi].Content[bi]
			block.Content = append([]llm.ContentBlock(nil), block.Content...)
			block.Content[0].Text = string(raw) + strings.TrimPrefix(b.Content[0].Text, text)
		}
	}
	return req
}
