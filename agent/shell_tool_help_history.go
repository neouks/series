package agent

import (
	"encoding/json"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
)

// Noa references identify different tool calls, not different help versions.
// Compare the complete page body without its trailing ref, then restore every
// original reference onto the projected block. The transcript is never edited.
func deduplicateShellHelpHistory(req llm.CompletionRequest) llm.CompletionRequest {
	ids := map[string]bool{}
	for _, m := range req.Messages {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse && b.Name == ShellToolHelpName {
				ids[b.ID] = true
			}
		}
	}
	if len(ids) == 0 {
		return req
	}
	type ref struct {
		m, b, c int
		suffix  string
	}
	var refs []ref
	for mi, m := range req.Messages {
		for bi, b := range m.Content {
			if b.Type != llm.BlockToolResult || !ids[b.ToolUseID] {
				continue
			}
			for ci, c := range b.Content {
				if c.Type != llm.BlockText {
					continue
				}
				plain := noaadapter.StripRefTag(c.Text)
				if plain != c.Text {
					refs = append(refs, ref{mi, bi, ci, c.Text[len(plain):]})
				}
			}
		}
	}
	if len(refs) > 0 {
		req = cloneToolMessages(req)
		for _, r := range refs {
			b := &req.Messages[r.m].Content[r.b]
			b.Content = append([]llm.ContentBlock(nil), b.Content...)
			b.Content[r.c].Text = noaadapter.StripRefTag(b.Content[r.c].Text)
		}
	}
	out := deduplicateReadHistoryBy(req, 1024, 1024, func(name string, raw json.RawMessage) string {
		if name != ShellToolHelpName {
			return ""
		}
		var input map[string]any
		if json.Unmarshal(raw, &input) != nil {
			return ""
		}
		key, err := json.Marshal(input)
		if err != nil {
			return ""
		}
		return name + ":" + string(key)
	})
	for _, r := range refs {
		out.Messages[r.m].Content[r.b].Content[r.c].Text += r.suffix
	}
	return out
}
