//go:build liveeval

package agent

// Frozen B/C controls from the previous evaluation. Do not use in production.
import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
	actool "github.com/Autumn-27/norma/tool"
	"reflect"
	"strconv"
	"strings"
)

type legacyDiscoveredContract struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Excerpt     string          `json:"description_excerpt"`
	Schema      json.RawMessage `json:"params_schema"`
	Rediscover  string          `json:"rediscover_query"`
}

const legacyToolDocumentationWindowRule = " 工具完整帮助只保留到紧接着的一次模型响应。延迟工具参数按最近发现顺序保留，最多8个、Schema合计24 KiB；相同工具仅保留最新一份，超限的旧定义省略。被省略、压缩或过期的参数必须先用SearchExtraTools精确查询select:工具名，看到返回的完整Schema后再调用；搜索与执行不能在同一轮猜参数。单个超大Schema须精确查询，仅完整保留到下一次响应。摘要不是完整用法，缺少语义或示例时也须重读。"

// Decode only the complete text protocol emitted by the pinned SDK. Unknown,
// truncated or ambiguous formats remain untouched: never guess a parameter
// contract from a partial result. Schemas remain lossless, including nested
// descriptions, defaults, enums, references and validation constraints.
func legacyDiscoveryContracts(text string) ([]legacyDiscoveredContract, bool) {
	header, body, ok := strings.Cut(text, "\n\n## ")
	if !ok {
		return nil, false
	}
	number, ok := strings.CutPrefix(header, "Found ")
	if !ok {
		return nil, false
	}
	number, ok = strings.CutSuffix(number, ` deferred tool(s). Invoke with ExecuteExtraTool({"tool_name":"<name>","params":{...}}):`)
	if !ok {
		return nil, false
	}
	count, err := strconv.Atoi(number)
	if err != nil || count <= 0 {
		return nil, false
	}
	out := []legacyDiscoveredContract{}
	seen := map[string]bool{}
	for body != "" {
		name, rest, ok := strings.Cut(body, "\n")
		if !ok || name == "" || seen[name] || len(out) >= count {
			return nil, false
		}
		// Markdown headings in a description are not entry boundaries. Only
		// a complete SDK schema line followed by EOF/the next header is one.
		at, schema, remaining := legacyDiscoverySchemaLine(rest)
		if at < 0 {
			return nil, false
		}
		seen[name] = true
		out = append(out, legacyDiscoveredContract{Name: name, Description: rest[:at], Excerpt: compactToolText(rest[:at], 220), Schema: json.RawMessage(schema), Rediscover: "select:" + name})
		body = remaining
	}
	return out, len(out) == count
}

func legacyDiscoverySchemaLine(text string) (int, string, string) {
	const marker = "\nparams schema: "
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], marker)
		if index < 0 {
			break
		}
		index += offset
		offset = index + len(marker)
		schema, tail, newline := strings.Cut(text[offset:], "\n")
		if !newline || !strings.HasPrefix(schema, "{") || !strings.HasSuffix(schema, "}") || !json.Valid([]byte(schema)) {
			continue
		}
		if tail == "" {
			return index, schema, ""
		}
		if next, ok := strings.CutPrefix(tail, "\n## "); ok && next != "" {
			return index, schema, next
		}
	}
	return -1, "", ""
}

const legacyHistoricalToolHelpNote = "完整说明已从本次请求的旧历史中省略，摘要不是完整用法。参数以保留的完整 Schema 或本轮常驻工具 Schema 为准；缺少用法、参数含义或 Schema 时先按重读入口重新查询，不猜测。"

func legacyCompactDocumentationResult(call llm.ContentBlock, text string) string {
	body := noaadapter.StripRefTag(text)
	var value any
	switch call.Name {
	case actool.SearchExtraToolsName:
		contracts, ok := legacyRetainedContracts(body)
		if !ok {
			return text
		}
		for i := range contracts {
			contracts[i].Description = ""
		}
		value = struct {
			Omitted bool                       `json:"documentation_omitted"`
			Note    string                     `json:"note"`
			Tools   []legacyDiscoveredContract `json:"tools"`
		}{true, legacyHistoricalToolHelpNote, contracts}
	case toolHelpName:
		var input struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(call.Input, &input) != nil || strings.TrimSpace(input.Name) == "" || json.Valid([]byte(body)) {
			return text // includes existing projection/dedup placeholders
		}
		value = map[string]any{"documentation_omitted": true, "name": input.Name, "note": legacyHistoricalToolHelpNote,
			"reread": map[string]any{"tool": toolHelpName, "params": map[string]string{"name": input.Name}}}
	default:
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) >= len(body) {
		return text
	}
	return string(encoded) + strings.TrimPrefix(text, body) // retain any noa ref suffix
}

// Full discovery/help results survive the immediate next model response, even
// for parallel tool batches. Once an assistant response follows a result, only
// its callable contract and reread path are sent. This is a stateless request
// projection: resumes work without a cache, and audit/SDK history stays intact.
func legacyWindowToolDocumentation(req llm.CompletionRequest) llm.CompletionRequest {
	lastAssistant := -1
	for i, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			lastAssistant = i
		}
	}
	pending := map[string]llm.ContentBlock{}
	seen := map[string]bool{}
	cloned := false
	for mi := 0; mi < lastAssistant; mi++ {
		m := req.Messages[mi]
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
				if b.Name == actool.SearchExtraToolsName || b.Name == toolHelpName {
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
			text := legacyCompactDocumentationResult(call, b.Content[0].Text)
			if text == b.Content[0].Text {
				continue
			}
			if !cloned {
				req = cloneToolMessages(req)
				cloned = true
			}
			result := &req.Messages[mi].Content[bi]
			result.Content = append([]llm.ContentBlock(nil), result.Content...)
			result.Content[0].Text = text
		}
	}
	return req
}

const legacyDiscoveredSchemaCount = 8
const legacyDiscoveredSchemaBytes = 24 << 10
const legacySchemaOmittedResult = `{"schema_omitted":true,"message":"该发现结果的参数定义已省略或无法确认完整性。先用 SearchExtraTools 的 select:工具名 精确查询，取得完整 Schema 后在下一轮执行；不能按旧调用猜参数。"}`

// Catalog tools can acquire a coverage/schema or interactive-session wrapper
// after OnDemandTools. These decorators must not remove the discovery gate.
func legacyRequiresDiscoveredSchema(tool actool.CoreTool) bool {
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

type legacyDiscoveryHistoryResult struct {
	message, block int
	call           llm.ContentBlock
	text           string
	fresh          bool
}

// Match successful tool results, never free-form user/assistant prose. This
// works on resumed history too; a session-wide 'ever discovered' bit would
// incorrectly authorize tools whose schemas have since been compacted away.
func legacyDiscoveryHistory(req llm.CompletionRequest) []legacyDiscoveryHistoryResult {
	lastAssistant := -1
	for i, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			lastAssistant = i
		}
	}
	pending := map[string]llm.ContentBlock{}
	seen := map[string]bool{}
	var results []legacyDiscoveryHistoryResult
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
			results = append(results, legacyDiscoveryHistoryResult{mi, bi, call, b.Content[0].Text, mi > lastAssistant})
		}
	}
	return results
}

type legacyRetainedDiscovery struct {
	DocumentationOmitted bool                       `json:"documentation_omitted"`
	Note                 string                     `json:"note"`
	Tools                []legacyDiscoveredContract `json:"tools"`
}

func legacyRetainedContracts(text string) ([]legacyDiscoveredContract, bool) {
	text = noaadapter.StripRefTag(text)
	if contracts, ok := legacyDiscoveryContracts(text); ok {
		return contracts, true
	}
	var value legacyRetainedDiscovery
	if json.Unmarshal([]byte(text), &value) != nil || !value.DocumentationOmitted || len(value.Tools) == 0 {
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

// Keep complete contracts in a bounded, newest-discovery-first window. A fresh
// exact single-tool lookup may exceed the byte budget for ONE model response;
// otherwise an inherently large schema could never be used without guessing.
func legacyBoundDiscoveredSchemas(req llm.CompletionRequest) llm.CompletionRequest {
	results := legacyDiscoveryHistory(req)
	seen := map[string]bool{}
	remaining, count := legacyDiscoveredSchemaBytes, 0
	cloned := false
	for i := len(results) - 1; i >= 0; i-- {
		r := results[i]
		body := noaadapter.StripRefTag(r.text)
		contracts, valid := legacyRetainedContracts(body)
		var kept []legacyDiscoveredContract
		if valid {
			var input struct {
				Query string `json:"query"`
			}
			_ = json.Unmarshal(r.call.Input, &input)
			for _, c := range contracts {
				if seen[c.Name] {
					continue
				}
				seen[c.Name] = true // never fall back to an older version of a definition
				oversize := r.fresh && len(contracts) == 1 && count == 0 && strings.TrimSpace(input.Query) == "select:"+c.Name
				if count >= legacyDiscoveredSchemaCount || (len(c.Schema) > remaining && !oversize) {
					continue
				}
				kept = append(kept, c)
				count++
				remaining -= len(c.Schema)
			}
		}
		if valid && len(kept) == len(contracts) {
			continue
		}
		// Preserve fresh failures/unknown diagnostics, but do not grant a lease
		// from them. Old opaque discovery text must not retain unbounded schemas.
		if !valid && (r.fresh || body == historicalReadOmitted || strings.HasPrefix(body, "No matching deferred tools for query:")) {
			continue
		}
		text := legacySchemaOmittedResult
		if len(kept) > 0 {
			encoded, err := json.Marshal(legacyRetainedDiscovery{true, "本轮仅保留以下工具的完整参数定义；未列出的命中项超出参数窗口，请用 SearchExtraTools 按名称精确重读。摘要不是完整用法，缺少说明时也须重读。", kept})
			if err != nil {
				continue
			}
			text = string(encoded)
		}
		text += strings.TrimPrefix(r.text, body)
		if text == r.text {
			continue
		}
		if !cloned {
			req = cloneToolMessages(req)
			cloned = true
		}
		result := &req.Messages[r.message].Content[r.block]
		result.Content = append([]llm.ContentBlock(nil), result.Content...)
		result.Content[0].Text = text
	}
	return req
}

// The request's schema must equal the current trusted execution definition.
// A stale snapshot, failed lookup, summary, or forged prose grants nothing.
func legacyMatchingSchema(raw json.RawMessage, schema map[string]any) bool {
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

func (h *dispatchHooks) legacyObserveDiscoveredSchemas(req llm.CompletionRequest) {
	visible := map[string]bool{}
	for _, r := range legacyDiscoveryHistory(req) {
		contracts, ok := legacyRetainedContracts(r.text)
		if !ok {
			continue
		}
		for _, c := range contracts {
			if tool := h.tools[c.Name]; tool != nil && h.deferred[c.Name] && legacyMatchingSchema(c.Schema, tool.InputSchema()) {
				visible[c.Name] = true
			}
		}
	}
	h.viewMu.Lock()
	h.schemaVisible = visible
	h.viewMu.Unlock()
}

// Project only the model request. Raw history, skill invocation side effects,
// transcript recovery and audit records are unchanged. Deduplicate identical
// guidance, then expire old full tool help while retaining parameter contracts.
func legacyProjectToolDocumentation(req llm.CompletionRequest) llm.CompletionRequest {
	req = governRequestDocs(deduplicateReadHistoryBy(req, 1024, 1024, func(name string, raw json.RawMessage) string {
		if name != "SearchExtraTools" && name != toolHelpName {
			return ""
		}
		var input struct {
			Query string `json:"query"`
			Name  string `json:"name"`
		}
		if json.Unmarshal(raw, &input) != nil {
			return ""
		}
		key := input.Query
		if name == toolHelpName {
			key = input.Name
		}
		if strings.TrimSpace(key) == "" {
			return ""
		}
		return name + ":" + strings.TrimSpace(key)
	}))
	seen := map[[32]byte]bool{}
	cloned := false
	for i := len(req.Messages) - 1; i >= 0; i-- {
		message := req.Messages[i]
		if _, ok := llm.SkillInvocationName(message); !ok {
			continue
		}
		text := message.Content[0].Text
		if len(text) > 256<<10 || len(seen) >= 1024 {
			continue
		}
		key := sha256.Sum256([]byte(text))
		if seen[key] {
			if !cloned {
				req = cloneToolMessages(req)
				cloned = true
			}
			req.Messages[i].Content[0] = llm.TextBlock("重复的 skill 指引已省略；后文保留相同正文，请复用已有指引。")
		} else {
			seen[key] = true
		}
	}
	return legacyBoundDiscoveredSchemas(legacyWindowToolDocumentation(req))
}
