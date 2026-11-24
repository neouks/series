package agent

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
)

// Only repetitive plain execution logs qualify. All values, line order and
// whitespace survive; evidence selection/summarization is deliberately absent.
var logAssignment = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.-]*=([^\s]+)`)

type logSegment struct {
	Text  string     `json:"text,omitempty"`
	Parts []string   `json:"parts,omitempty"`
	Rows  [][]string `json:"rows,omitempty"`
}
type losslessLog struct {
	Format   string       `json:"format"`
	Rule     string       `json:"rule"`
	Segments []logSegment `json:"segments"`
}

func splitLogLine(line string) ([]string, []string) {
	if len(line) > 4096 {
		return nil, nil
	}
	matches := logAssignment.FindAllStringSubmatchIndex(line, 9)
	if len(matches) > 8 {
		return nil, nil
	}
	var parts, values []string
	last := 0
	for _, m := range matches {
		value := line[m[2]:m[3]]
		if !strings.ContainsFunc(value, unicode.IsDigit) {
			continue
		}
		parts = append(parts, line[last:m[2]])
		values = append(values, value)
		last = m[3]
	}
	if len(values) == 0 {
		return nil, nil
	}
	parts = append(parts, line[last:])
	return parts, values
}

func restoreLog(segments []logSegment) string {
	var out strings.Builder
	for _, s := range segments {
		if s.Parts == nil {
			out.WriteString(s.Text)
			continue
		}
		for _, row := range s.Rows {
			if len(s.Parts) != len(row)+1 {
				return ""
			}
			for i, v := range row {
				out.WriteString(s.Parts[i])
				out.WriteString(v)
			}
			out.WriteString(s.Parts[len(row)])
		}
	}
	return out.String()
}

// Factor common byte spans only at UTF-8 boundaries. The middle of EVERY line
// remains in order, including exceptions; this never selects evidence lines.
func commonLogGroup(lines []string, start int) (logSegment, int) {
	first := lines[start]
	if len(first) > 4096 || start+1 >= len(lines) {
		return logSegment{}, start
	}
	prefix, suffix := len(first), 0
	end := start + 1
	for end < min(len(lines), start+512) {
		next := lines[end]
		if len(next) > 4096 {
			break
		}
		p := 0
		for p < min(prefix, len(next)) && first[p] == next[p] {
			p++
		}
		for p > 0 && p < len(first) && !utf8.RuneStart(first[p]) {
			p--
		}
		s := 0
		maxSuffix := min(len(first)-p, len(next)-p)
		if end > start+1 {
			maxSuffix = min(maxSuffix, suffix)
		}
		for s < maxSuffix && first[len(first)-1-s] == next[len(next)-1-s] {
			s++
		}
		for s > 0 && !utf8.RuneStart(first[len(first)-s]) {
			s--
		}
		if p+s < 16 {
			break
		}
		prefix, suffix = p, s
		end++
	}
	if end-start < 8 {
		return logSegment{}, start
	}
	rows := make([][]string, 0, end-start)
	rawBytes := 0
	for _, line := range lines[start:end] {
		rows = append(rows, []string{line[prefix : len(line)-suffix]})
		rawBytes += len(line)
	}
	group := logSegment{Parts: []string{first[:prefix], first[len(first)-suffix:]}, Rows: rows}
	encoded, _ := json.Marshal(group)
	if len(encoded)*100 > rawBytes*85 {
		// Consume the inspected run without compression. Retrying the same
		// unprofitable 512-line suffix at every line would be quadratic.
		return logSegment{}, end
	}
	return group, end
}

func compactExecutionLog(text string) string {
	if len(text) < 12000 || len(text) > 4<<20 || !utf8.ValidString(text) || json.Valid([]byte(text)) {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	var segments []logSegment
	var pending strings.Builder
	literal := func(s string) { pending.WriteString(s) }
	flush := func() {
		if pending.Len() > 0 {
			segments = append(segments, logSegment{Text: pending.String()})
			pending.Reset()
		}
	}
	changed := false
	for i := 0; i < len(lines); {
		parts, values := splitLogLine(lines[i])
		if parts == nil {
			if group, end := commonLogGroup(lines, i); end > i {
				if group.Parts == nil {
					literal(strings.Join(lines[i:end], ""))
				} else {
					flush()
					segments = append(segments, group)
					changed = true
				}
				i = end
				continue
			}
			literal(lines[i])
			i++
			continue
		}
		j := i + 1
		rows := [][]string{values}
		rawBytes := len(lines[i])
		for j < len(lines) {
			p, v := splitLogLine(lines[j])
			if !slices.Equal(parts, p) {
				break
			}
			rows = append(rows, v)
			rawBytes += len(lines[j])
			j++
		}
		group := logSegment{Parts: parts, Rows: rows}
		encoded, _ := json.Marshal(group)
		if len(rows) >= 8 && len(encoded)*100 <= rawBytes*85 {
			flush()
			segments = append(segments, group)
			changed = true
		} else {
			if group, end := commonLogGroup(lines, i); end > i {
				if group.Parts == nil {
					literal(strings.Join(lines[i:end], ""))
				} else {
					flush()
					segments = append(segments, group)
					changed = true
				}
				i = end
				continue
			}
			literal(strings.Join(lines[i:j], ""))
		}
		i = j
	}
	flush()
	if !changed || restoreLog(segments) != text {
		return text
	}
	encoded, _ := json.Marshal(losslessLog{Format: "lossless_log_v1", Rule: "完整原文的无损表示，没有省略行。text是原文；rows每行按parts[0]+row[0]+parts[1]+…+parts末项拼接，顺序及所有值均保留。这里只共享重复格式，不是摘要或结论。", Segments: segments})
	if len(encoded)*100 > len(text)*85 {
		return text
	}
	return string(encoded)
}

// Model-view only, after authorization and exact-result deduplication. No
// changes to reasoning, tool arguments, call/result IDs, audit or read tools.
func compactExecutionLogHistory(req llm.CompletionRequest) llm.CompletionRequest {
	// A tight output budget can be consumed by interpreting an alternate
	// representation. Preserve the familiar original format in that case.
	// Zero means the provider chooses its default, so the available budget is
	// unknown. Preserve the original in that case too; never raise the budget.
	if req.MaxTokens < 8192 {
		return req
	}
	calls := map[string]string{}
	changed := false
	for mi, m := range req.Messages {
		if m.Role == llm.RoleAssistant {
			clear(calls)
		}
		for bi, b := range m.Content {
			if m.Role == llm.RoleAssistant && b.Type == llm.BlockToolUse {
				c := assetContextCall(b)
				calls[c.ID] = c.Name
				continue
			}
			if b.Type != llm.BlockToolResult {
				continue
			}
			name := calls[b.ToolUseID]
			delete(calls, b.ToolUseID)
			if b.IsError || len(b.Content) != 1 || b.Content[0].Type != llm.BlockText {
				continue
			}
			switch name {
			case "Bash", "Monitor", "Grep", "Glob", "LS":
			default:
				continue
			}
			old := b.Content[0].Text
			plain := noaadapter.StripRefTag(old)
			next := compactExecutionLog(plain)
			if plain == next {
				continue
			}
			if !changed {
				req = cloneToolMessages(req)
				changed = true
			}
			r := &req.Messages[mi].Content[bi]
			r.Content = slices.Clone(r.Content)
			r.Content[0].Text = next + old[len(plain):]
		}
	}
	return req
}
