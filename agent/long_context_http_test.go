package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

func TestProjectedHistoryFinalOpenAIJSONPairing(t *testing.T) {
	for _, noa := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("noa=%v/stream=%v", noa, stream), func(t *testing.T) {
				var requests atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					var body struct {
						Stream   bool `json:"stream"`
						Messages []struct {
							Role       string `json:"role"`
							ToolCallID string `json:"tool_call_id"`
							ToolCalls  []struct {
								ID string `json:"id"`
							} `json:"tool_calls"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					pending := map[string]bool{}
					callCount := 0
					for _, m := range body.Messages {
						if m.Role != "tool" && len(pending) > 0 {
							t.Error("tool results crossed an ordinary message")
							w.WriteHeader(400)
							return
						}
						if m.Role == "tool" {
							if !pending[m.ToolCallID] {
								t.Error("orphan/duplicate tool result")
								w.WriteHeader(400)
								return
							}
							delete(pending, m.ToolCallID)
						}
						for _, call := range m.ToolCalls {
							if call.ID == "" || pending[call.ID] {
								t.Error("empty/duplicate call id")
								w.WriteHeader(400)
								return
							}
							pending[call.ID] = true
							callCount++
						}
					}
					if len(pending) > 0 || callCount != 2 {
						t.Errorf("pending=%v retained_calls=%d", pending, callCount)
						w.WriteHeader(400)
						return
					}
					if body.Stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":123,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":123,"completion_tokens":3}}`)
					}
				}))
				defer server.Close()
				provider, err := (Config{Format: llm.FormatOpenAI, BaseURL: server.URL, Model: "fixture"}).NewProvider()
				if err != nil {
					t.Fatal(err)
				}
				req := retrievalFixture(100)
				original := req
				before, _ := json.Marshal(req)
				if noa {
					opts := agentcore.Options{}
					enableNoa(&opts, func() bool { return true }, t.TempDir(), "long-context-http", nil)
					req.Messages = opts.Compactor.(harness.ContextView).View(t.Context(), req.Messages)
				}
				ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "worker", TaskID: 1, IntentID: 7})
				wrapped := withAssetContext(provider, nil, 1)
				if stream {
					for _, err = range wrapped.Stream(ctx, req) {
						if err != nil {
							t.Fatal(err)
						}
					}
				} else {
					if _, _, _, err = wrapped.Complete(ctx, req); err != nil {
						t.Fatal(err)
					}
				}
				if requests.Load() != 1 {
					t.Fatal("unexpected repeated model request", requests.Load())
				}
				after, _ := json.Marshal(original)
				if string(after) != string(before) {
					t.Fatal("original audit request changed")
				}
			})
		}
	}
}
