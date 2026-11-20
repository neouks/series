package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Autumn-27/norma/harness"
	"reflect"
	"sort"
	"strings"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
	actool "github.com/Autumn-27/norma/tool"
)

const schemaOmittedResult = `{"schema_omitted":true,"message":"该发现结果的参数定义已省略或无法确认完整性。先用 SearchExtraTools 的 select:工具名 精确查询，取得完整 Schema 后在下一轮执行；不能按旧调用猜参数。"}`

// Catalog tools can acquire a coverage/schema or interactive-session wrapper
// after OnDemandTools. These decorators must not remove the discovery gate.
func requiresDiscoveredSchema(tool actool.CoreTool) bool {
	for tool != nil {
		switch t := tool.(type) {
		case *discoveredTool:
			return true
		case *overriddenTool:
			tool = t.CoreTool
		case *guardedTool:
			tool = t.CoreTool
		case *interactiveTool:
			tool = t.CoreTool
		case *processContextTool:
			tool = t.CoreTool
		default:
			return false
		}
	}
	return false
}

type discoveryHistoryResult struct {
	message, block int
	call           llm.ContentBlock
	text           string
	fresh          bool
}

// Match successful tool results, never free-form user/assistant prose. This
// works on resumed history too; a session-wide 'ever discovered' bit would
// incorrectly authorize tools whose schemas have since been compacted away.
func discoveryHistory(req llm.CompletionRequest) []discoveryHistoryResult {
	lastAssistant := -1
	for i, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			lastAssistant = i
		}
	}
	pending := map[string]llm.ContentBlock{}
	seen := map[string]bool{}
	var results []discoveryHistoryResult
	for mi, m := range req.Messages {
		if m.Role == llm.RoleAssistant || m.Role == "system" {
			clear(pending)
			clear(seen)
		}
		for bi, b := range m.Content {
			if b.Type == llm.BlockToolUse && m.Role == llm.RoleAssistant {
				delete(pending, b.ID)
				if b.ID == "" || len(b.ID) > 1024 || seen[b.ID] || len(seen) >= 1024 || len(b.Input) > historicalReadInputBytes {
					continue
				}
				seen[b.ID] = true
				if b.Name == actool.SearchExtraToolsName {
					pending[b.ID] = b
				}
				continue
			}
			if b.Type != llm.BlockToolResult {
				if m.Role == llm.RoleUser {
					clear(pending)
					clear(seen)
				}
				continue
			}
			call, ok := pending[b.ToolUseID]
			delete(pending, b.ToolUseID)
			if !ok || b.IsError || len(b.Content) != 1 || b.Content[0].Type != llm.BlockText {
				continue
			}
			results = append(results, discoveryHistoryResult{mi, bi, call, b.Content[0].Text, mi > lastAssistant})
		}
	}
	return results
}

type retainedDiscovery struct {
	DocumentationOmitted bool                 `json:"documentation_omitted,omitempty"`
	ContractAvailable    bool                 `json:"contract_available,omitempty"`
	Note                 string               `json:"note"`
	Tools                []discoveredContract `json:"tools"`
}

func retainedContracts(text string) ([]discoveredContract, bool) {
	text = noaadapter.StripRefTag(text)
	if contracts, ok := discoveryContracts(text); ok {
		return contracts, true
	}
	var value retainedDiscovery
	if json.Unmarshal([]byte(text), &value) != nil || (!value.DocumentationOmitted && !value.ContractAvailable) || len(value.Tools) == 0 {
		return nil, false
	}
	seen := map[string]bool{}
	for _, c := range value.Tools {
		if c.Name == "" || seen[c.Name] || !bytes.HasPrefix(c.Schema, []byte("{")) || !json.Valid(c.Schema) {
			return nil, false
		}
		seen[c.Name] = true
	}
	return value.Tools, true
}

// A window belongs to the session. Evicted result identities stay evicted until
// an explicit new discovery; lower pressure cannot silently resurrect old data.
// Prune identities when raw history/compaction removes their result.
type toolSchemaPressure struct {
	window  int
	evicted map[string]bool
}

type contextWindowCompactor struct {
	harness.Compactor
	window int
}

func (c *contextWindowCompactor) View(ctx context.Context, messages []llm.Message) []llm.Message {
	if v, ok := c.Compactor.(harness.ContextView); ok {
		return v.View(ctx, messages)
	}
	return messages
}

// Estimate the WHOLE serialized request, including system, resident schemas and
// ordinary results. Bytes/3 is a conservative trigger, not exact token accounting.
// Reserve output plus 20% headroom, evict to 65% to avoid one eviction each turn.
func requestInputEstimate(req llm.CompletionRequest) int {
	raw, _ := json.Marshal(req)
	return (len(raw) + 2) / 3
}

func boundDiscoveredSchemas(req llm.CompletionRequest) llm.CompletionRequest {
	return (&toolSchemaPressure{window: defaultWindowK * 1000}).project(req)
}

func (p *toolSchemaPressure) project(req llm.CompletionRequest) llm.CompletionRequest {
	results := discoveryHistory(req)
	type entry struct {
		result   int
		contract discoveredContract
		key      string
		keep     bool
		used     int
	}
	var entries []*entry
	groups := make([][]*entry, len(results))
	valid := make([]bool, len(results))
	seen := map[string]bool{}
	live := map[string]bool{}
	for _, r := range results {
		live[r.call.ID] = true
	}
	lastUse := map[string]int{}
	for mi, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			for _, b := range m.Content {
				if b.Type != llm.BlockToolUse {
					continue
				}
				name := b.Name
				if name == actool.ExecuteExtraToolName {
					var in struct {
						Name string `json:"tool_name"`
					}
					_ = json.Unmarshal(b.Input, &in)
					name = in.Name
				}
				lastUse[name] = mi
			}
		}
	}
	for i := len(results) - 1; i >= 0; i-- {
		contracts, ok := retainedContracts(results[i].text)
		valid[i] = ok
		for _, c := range contracts {
			key := results[i].call.ID + "\x00" + c.Name

			e := &entry{result: i, contract: c, key: key, keep: !seen[c.Name] && !p.evicted[key], used: max(results[i].message, lastUse[c.Name])}
			seen[c.Name] = true
			entries = append(entries, e)
			groups[i] = append(groups[i], e)
		}
	}
	for key := range p.evicted {
		id, _, _ := strings.Cut(key, "\x00")
		if !live[id] {
			delete(p.evicted, key)
		}
	}
	render := func() llm.CompletionRequest {
		out := req
		cloned := false
		for i, r := range results {
			body := noaadapter.StripRefTag(r.text)
			if !valid[i] && (r.fresh || body == historicalReadOmitted || body == schemaOmittedResult || strings.HasPrefix(body, "No matching deferred tools for query:")) {
				continue
			}
			var kept []discoveredContract
			for _, e := range groups[i] {
				if e.keep {
					kept = append(kept, e.contract)
				}
			}
			if valid[i] && len(kept) == len(groups[i]) {
				continue
			}
			text := schemaOmittedResult
			if len(kept) > 0 {
				raw, _ := json.Marshal(retainedDiscovery{ContractAvailable: true, Note: historicalToolHelpNote, Tools: kept})
				text = string(raw)
			}
			text += strings.TrimPrefix(r.text, body)
			if text == r.text {
				continue
			}
			if !cloned {
				out = cloneToolMessages(req)
				cloned = true
			}
			result := &out.Messages[r.message].Content[r.block]
			result.Content = append([]llm.ContentBlock(nil), result.Content...)
			result.Content[0].Text = text
		}
		return out
	}
	out := render()
	window := p.window
	if window <= 0 {
		window = defaultWindowK * 1000
	}
	reserve := req.MaxTokens
	if reserve <= 0 {
		reserve = min(8192, window/5)
	}
	high := max(1, window*80/100-reserve)
	if requestInputEstimate(out) <= high {
		return out
	}
	target := max(1, window*65/100-reserve)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].used == entries[j].used {
			return entries[i].result < entries[j].result
		}
		return entries[i].used < entries[j].used
	})
	// An exact fresh single-tool lookup gets one response, even if that contract
	// alone is oversized. Existing model-capacity handling remains authoritative.
	for _, e := range entries {
		if !e.keep {
			continue
		}
		r := results[e.result]
		var input struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(r.call.Input, &input)
		if r.fresh && len(groups[e.result]) == 1 && strings.TrimSpace(input.Query) == "select:"+e.contract.Name {
			continue
		}
		e.keep = false
		if p.evicted == nil {
			p.evicted = map[string]bool{}
		}
		p.evicted[e.key] = true
		out = render()
		if requestInputEstimate(out) <= target {
			break
		}
	}
	return out
}

// The request's schema must equal the current trusted execution definition.
// A stale snapshot, failed lookup, summary, or forged prose grants nothing.
func matchingSchema(raw json.RawMessage, schema map[string]any) bool {
	want, err := json.Marshal(schema)
	if err != nil {
		return false
	}
	decode := func(raw []byte) (any, error) {
		var value any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		err := d.Decode(&value)
		return value, err
	}
	a, errA := decode(raw)
	b, errB := decode(want)
	return errA == nil && errB == nil && reflect.DeepEqual(a, b)
}

func (h *dispatchHooks) observeDiscoveredSchemas(req llm.CompletionRequest) {
	visible := map[string]bool{}
	for _, r := range discoveryHistory(req) {
		contracts, ok := retainedContracts(r.text)
		if !ok {
			continue
		}
		for _, c := range contracts {
			if tool := h.tools[c.Name]; tool != nil && h.deferred[c.Name] && matchingSchema(c.Schema, tool.InputSchema()) && compactToolGuidance(c.Description) == compactToolGuidance(tool.Description()) {
				visible[c.Name] = true
			}
		}
	}
	h.viewMu.Lock()
	h.schemaVisible = visible
	h.viewMu.Unlock()
}

// The SDK caps every tool result before it reaches the model hook. An exact,
// successful discovery may therefore contain only the head/tail of a schema or
// guidance. Rehydrate only that specifically requested, registered definition;
// never infer definitions from prose, failed searches or a broad truncated batch.
// The normal pressure policy and permission/unlock gates still apply afterward.
func (h *dispatchHooks) projectToolDocumentation(req llm.CompletionRequest) llm.CompletionRequest {
	cloned := false
	for _, r := range discoveryHistory(req) {
		body := noaadapter.StripRefTag(r.text)
		if !strings.Contains(body, "characters truncated]") && !strings.Contains(body, "<persisted-output>") {
			continue
		}
		var input struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(r.call.Input, &input) != nil {
			continue
		}
		name, exact := strings.CutPrefix(strings.TrimSpace(input.Query), "select:")
		tool := h.tools[name]
		if !exact || tool == nil || !h.deferred[name] {
			continue
		}
		header := fmt.Sprintf("Found 1 deferred tool(s). Invoke with ExecuteExtraTool({\"tool_name\":\"<name>\",\"params\":{...}}):\n\n## %s\n", name)
		if !strings.HasPrefix(body, header) {
			continue
		}
		schema, err := json.Marshal(tool.InputSchema())
		if err != nil {
			continue
		}
		text := header + tool.Description() + "\nparams schema: " + string(schema) + "\n" + strings.TrimPrefix(r.text, body)
		if !cloned {
			req = cloneToolMessages(req)
			cloned = true
		}
		result := &req.Messages[r.message].Content[r.block]
		result.Content = append([]llm.ContentBlock(nil), result.Content...)
		result.Content[0].Text = text
	}
	return h.schemaPressure.projectDocumentation(req)
}
