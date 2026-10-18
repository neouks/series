package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// Exercise the actual pinned SDK through SERIES provider construction. These
// assertions document upstream semantics rather than reinstating fork patches.
func TestOfficialOpenAIWireHistoryAndSignatures(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var body struct {
				Messages []struct {
					Role      string `json:"role"`
					Reasoning string `json:"reasoning_content"`
					ToolID    string `json:"tool_call_id"`
					Calls     []struct {
						ID    string `json:"id"`
						Extra struct {
							Google struct {
								Signature string `json:"thought_signature"`
							} `json:"google"`
						} `json:"extra_content"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":3}}`)
				}
			}))
			defer srv.Close()
			provider, err := (Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "fixture", Model: "fixture", ThinkingType: "enabled"}).NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			call := func(id, sig string) llm.ContentBlock {
				return llm.ContentBlock{Type: llm.BlockToolUse, ID: id, Name: "context_get", Input: json.RawMessage(`{}`), ThoughtSignature: sig}
			}
			history := []llm.Message{
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "真实推理"}, call("kept", "model-signature"), call("missing", "")}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("kept", "result", false), llm.ToolResultText("orphan", "drop", false)}},
				{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call("later", "")}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock("inserted summary")}},
				{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("later", "late result", false)}},
			}
			original, _ := json.Marshal(history)
			req := llm.CompletionRequest{Messages: history, MaxTokens: 256}
			if stream {
				for _, err := range provider.Stream(t.Context(), req) {
					if err != nil {
						t.Fatal(err)
					}
				}
			} else {
				_, _, usage, err := provider.Complete(t.Context(), req)
				if err != nil || usage.InputTokens != 17 {
					t.Fatalf("usage=%+v err=%v", usage, err)
				}
			}
			if len(body.Messages) != 5 || len(body.Messages[0].Calls) != 1 || body.Messages[0].Calls[0].ID != "kept" || body.Messages[0].Calls[0].Extra.Google.Signature != "model-signature" || body.Messages[0].Reasoning != "真实推理" {
				t.Fatalf("pairing/signature changed: %+v", body)
			}
			// Official pairing scans the entire history; unlike the removed local
			// patch it retains this cross-message pair. Do not silently claim repair.
			if body.Messages[2].Calls[0].ID != "later" || body.Messages[2].Reasoning == "" || body.Messages[3].Role != "user" || body.Messages[4].ToolID != "later" {
				t.Fatalf("upstream behavior changed: %+v", body)
			}
			after, _ := json.Marshal(history)
			if string(after) != string(original) || strings.Contains(string(after), "reasoning elided") {
				t.Fatal("original history mutated")
			}
		})
	}
}
