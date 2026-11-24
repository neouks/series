package agent

import (
	"crypto/sha256"
	"encoding/json"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

// Project only the model request. Raw history, skill invocation side effects,
// transcript recovery and audit records are unchanged. Deduplicate identical
// guidance, then expire old full tool help while retaining parameter contracts.
func projectToolDocumentation(req llm.CompletionRequest) llm.CompletionRequest {
	return (&toolSchemaPressure{window: defaultWindowK * 1000}).projectDocumentation(req)
}

func (p *toolSchemaPressure) projectDocumentation(req llm.CompletionRequest) llm.CompletionRequest {
	req = deduplicateShellHelpHistory(req)
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
	return p.project(windowToolDocumentation(req))
}
