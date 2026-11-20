package agent

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
	actool "github.com/Autumn-27/norma/tool"
)

type discoveredContract struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Excerpt     string          `json:"description_excerpt,omitempty"`
	Schema      json.RawMessage `json:"params_schema"`
	Rediscover  string          `json:"rediscover_query"`
}

const toolDocumentationWindowRule = " 已发现工具的完整参数Schema和用法约束可直接复用，不必因执行过或参考段落去重而重复查询。仅在当前请求缺少定义、定义已过期或标记schema_omitted时，先用SearchExtraTools精确查询select:工具名，收到结果后再执行，不能在同一轮搜索并猜参数。Schema只在请求接近上下文预算时淘汰，工具数量本身不触发淘汰。"

// Decode only the complete text protocol emitted by the pinned SDK. Unknown,
// truncated or ambiguous formats remain untouched: never guess a parameter
// contract from a partial result. Schemas remain lossless, including nested
// descriptions, defaults, enums, references and validation constraints.
func discoveryContracts(text string) ([]discoveredContract, bool) {
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
	out := []discoveredContract{}
	seen := map[string]bool{}
	for body != "" {
		name, rest, ok := strings.Cut(body, "\n")
		if !ok || name == "" || seen[name] || len(out) >= count {
			return nil, false
		}
		// Markdown headings in a description are not entry boundaries. Only
		// a complete SDK schema line followed by EOF/the next header is one.
		at, schema, remaining := discoverySchemaLine(rest)
		if at < 0 {
			return nil, false
		}
		seen[name] = true
		out = append(out, discoveredContract{Name: name, Description: rest[:at], Excerpt: compactToolText(rest[:at], 220), Schema: json.RawMessage(schema), Rediscover: "select:" + name})
		body = remaining
	}
	return out, len(out) == count
}

func discoverySchemaLine(text string) (int, string, string) {
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

const historicalToolHelpNote = "完整Schema与用法约束可直接复用；仅连续重复的参考段落折叠并标明次数，未截取或概括独有规则。"

// No semantic summarizer: arbitrary prose can contain business constraints even
// without words such as 'must'. Preserve every distinct paragraph and all code.
// Only collapse adjacent, byte-identical plain prose paragraphs, with a count.
func compactToolGuidance(text string) string {
	if strings.Contains(text, "```") || strings.Contains(text, "~~~") {
		return text
	}
	parts := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(parts); {
		line := parts[i]
		end := i + 1
		// Exclude code indentation, lists, headings and short/count-like commands.
		plain := len([]rune(line)) >= 80 && line == strings.TrimSpace(line) && !strings.ContainsAny(line[:min(1, len(line))], "#-*+>|0123456789")
		if plain {
			for end < len(parts) && parts[end] == line {
				end++
			}
		}
		out = append(out, line)
		if end-i > 1 {
			out = append(out, "[上段原文连续重复 "+strconv.Itoa(end-i)+" 次；此处仅展示一次。]")
		}
		i = end
	}
	return strings.Join(out, "\n")
}

func compactDocumentationResult(call llm.ContentBlock, text string) string {
	body := noaadapter.StripRefTag(text)
	var value any
	switch call.Name {
	case actool.SearchExtraToolsName:
		contracts, ok := retainedContracts(body)
		if !ok {
			return text
		}
		for i := range contracts {
			contracts[i].Description = compactToolGuidance(contracts[i].Description)
			contracts[i].Excerpt = ""
		}
		value = retainedDiscovery{ContractAvailable: true, Note: historicalToolHelpNote, Tools: contracts}
	case toolHelpName:
		var input struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(call.Input, &input) != nil || strings.TrimSpace(input.Name) == "" || json.Valid([]byte(body)) {
			return text // includes existing projection/dedup placeholders
		}
		value = map[string]any{"contract_available": true, "name": input.Name, "guidance": compactToolGuidance(body)}
	default:
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) >= len(body) {
		return text
	}
	return string(encoded) + strings.TrimPrefix(text, body) // retain any noa ref suffix
}

// Project immediately to a stable, complete guidance representation. Later
// responses reuse identical bytes, avoiding a fresh-to-historical prefix rewrite.
// Raw SDK/audit messages are never mutated.
func windowToolDocumentation(req llm.CompletionRequest) llm.CompletionRequest {
	pending := map[string]llm.ContentBlock{}
	seen := map[string]bool{}
	cloned := false
	for mi := 0; mi < len(req.Messages); mi++ {
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
			text := compactDocumentationResult(call, b.Content[0].Text)
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
