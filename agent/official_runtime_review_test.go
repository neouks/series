package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// Upstream's failing test constructs a raw config without HTTPClient. Production
// uses Config.NewProvider; verify its actual retry path, not just construction.
func TestOfficialRuntimeReviewConfiguredHTTPRetries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer srv.Close()
			p, err := (Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, Model: "fixture", Retry: RetryConfig{ConnectAttempts: 1, ConnectInterval: time.Millisecond}}).NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			req := llm.CompletionRequest{Messages: []llm.Message{llm.UserText("fixture")}}
			if stream {
				for _, e := range p.Stream(t.Context(), req) {
					if e != nil {
						err = e
					}
				}
			} else {
				_, _, _, err = p.Complete(t.Context(), req)
			}
			if err == nil || calls.Load() != 2 {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

// Document the pinned upstream limit without changing its recovery algorithm.
// Plain text cannot be reclaimed by noa's tool-result valve. With an overstated
// window the run must terminate, not keep reissuing the rejected request.
func TestOfficialRuntimeReviewNoaOverflowStops(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			calls := 0
			opts := agentcore.Options{NonStreaming: !stream, DisableBackgroundTasks: true, Compaction: &compaction.Config{ContextWindow: 200000},
				Deps: reviewModelDeps(func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					return llm.Message{}, "", llm.Usage{}, errors.New("openai: status 400: maximum context length is 128000 tokens")
				})}
			enableNoa(&opts, func() bool { return true }, t.TempDir(), "overflow", nil)
			_, reason, err := captureRun(t.Context(), opts, strings.Repeat("context ", 80000), nil)
			if err == nil || reason != harness.ReasonPromptTooLong || calls != 1 {
				t.Fatalf("requests=%d reason=%s err=%v", calls, reason, err)
			}
			t.Logf("requests=%d terminal=%s; official recovery cannot shrink this plain-text history", calls, reason)
		})
	}
}
