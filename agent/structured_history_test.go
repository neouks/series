package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestUnverifiableHistoryPreservesUsableContextAndDiagnostics(t *testing.T) {
	for _, name := range []string{"asset_get", "context_get", "traffic_get", "list_assets"} {
		t.Run(name, func(t *testing.T) {
			var history []llm.Message
			for i, body := range []string{`{"assets":[{"id":1,"detail":"KEEP_VALID"}]}`, `{"assets":[{"detail":"DROP_BROKEN...`} {
				id := string(rune('a' + i))
				in, _ := json.Marshal(map[string]any{"tool_name": name, "params": map[string]any{"ids": []int{1}}})
				history = append(history,
					llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: actool.ExecuteExtraToolName, Input: in}}},
					llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, body, false)}})
			}
			before, _ := json.Marshal(history)
			out := deduplicateToolHistory(redactUnverifiableStructuredHistory(llm.CompletionRequest{Messages: history}))
			encoded, _ := json.Marshal(out.Messages)
			if strings.Contains(string(encoded), "DROP_BROKEN") || !strings.Contains(string(encoded), "KEEP_VALID") {
				t.Fatalf("context incorrectly replaced: %s", encoded)
			}
			after, _ := json.Marshal(history)
			if string(before) != string(after) {
				t.Fatal("audit modified")
			}
		})
	}
	for _, sample := range []struct {
		name    string
		isError bool
	}{{"Bash", false}, {"mcp__fixture", false}, {"context_get", true}} {
		history := []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "a", Name: sample.name}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("a", "ordinary text diagnostic", sample.isError)}},
		}
		out := redactUnverifiableStructuredHistory(llm.CompletionRequest{Messages: history})
		if blocksText(out.Messages[1].Content[0].Content) != "ordinary text diagnostic" {
			t.Fatal("altered ordinary output", sample)
		}
	}
}
