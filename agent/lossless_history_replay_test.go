package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestLosslessHistoryRecordedReplay(t *testing.T) {
	dir := os.Getenv("ARTEX_HISTORY_REPLAY")
	if dir == "" {
		t.Skip("requires captured request directory")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatal("missing requests")
	}
	requests, changed, eligible, oldBytes, newBytes := 0, 0, 0, 0, 0
	for _, path := range paths {
		var record struct {
			Worker string `json:"worker"`
			Raw    string `json:"raw_request"`
		}
		raw, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(raw, &record) != nil {
			t.Fatal("bad record")
		}
		if record.Worker != "worker" {
			continue
		}
		requests++
		var request struct {
			MaxTokens           int `json:"max_tokens"`
			MaxCompletionTokens int `json:"max_completion_tokens"`
			Messages            []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				ID      string          `json:"tool_call_id"`
				Calls   []struct {
					ID       string `json:"id"`
					Function struct {
						Name string `json:"name"`
						Args string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if json.Unmarshal([]byte(record.Raw), &request) != nil {
			t.Fatal("bad request")
		}
		req := llm.CompletionRequest{MaxTokens: request.MaxTokens}
		if request.MaxCompletionTokens > 0 {
			req.MaxTokens = request.MaxCompletionTokens
		}
		for _, m := range request.Messages {
			msg := llm.Message{Role: llm.Role(m.Role)}
			var text string
			_ = json.Unmarshal(m.Content, &text)
			if m.Role == "tool" {
				msg.Role = llm.RoleUser
				msg.Content = []llm.ContentBlock{llm.ToolResultText(m.ID, text, false)}
			} else {
				if text != "" {
					msg.Content = append(msg.Content, llm.TextBlock(text))
				}
				for _, call := range m.Calls {
					msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockToolUse, Name: call.Function.Name, ID: call.ID, Input: json.RawMessage(call.Function.Args)})
				}
			}
			req.Messages = append(req.Messages, msg)
		}
		projected := compactExecutionLogHistory(req)
		anyChange := false
		for i, m := range req.Messages {
			for j, b := range m.Content {
				if b.Type != llm.BlockToolResult {
					continue
				}
				old := b.Content[0].Text
				new := projected.Messages[i].Content[j].Content[0].Text
				oldBytes += len(old)
				newBytes += len(new)
				if old != new {
					eligible++
					anyChange = true
					var log losslessLog
					if json.Unmarshal([]byte(new), &log) != nil || restoreLog(log.Segments) != old {
						t.Fatal("replay is lossy")
					}
				}
			}
		}
		if anyChange {
			changed++
		}
	}
	report := map[string]any{"requests": requests, "changed_requests": changed, "changed_results": eligible, "tool_result_bytes_before": oldBytes, "tool_result_bytes_after": newBytes, "note": "sampled raw OpenAI worker requests; bytes, not billed tokens; raw protocol lacks IsError so coverage is an upper bound"}
	raw, _ := json.MarshalIndent(report, "", "  ")
	t.Log(string(raw))
	if out := os.Getenv("ARTEX_HISTORY_REPLAY_OUT"); out != "" {
		if err := os.WriteFile(out, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
