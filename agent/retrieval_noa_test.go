package agent

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
)

type retrievalProbeProvider struct {
	llm.Provider
	check func(llm.CompletionRequest)
}

func (p retrievalProbeProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.check(req)
	return llm.Message{}, "end_turn", llm.Usage{}, nil
}
func (p retrievalProbeProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.check(req)
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func TestRetrievalNoaAndProviderPaths(t *testing.T) {
	for _, noa := range []bool{false, true} {
		req := retrievalFixture(20)
		if noa {
			opts := agentcore.Options{}
			enableNoa(&opts, func() bool { return true }, t.TempDir(), "retrieval", nil)
			req.Messages = opts.Compactor.(harness.ContextView).View(t.Context(), req.Messages)
		}
		before, _ := json.Marshal(req)
		calls := 0
		provider := withAssetContext(retrievalProbeProvider{check: func(got llm.CompletionRequest) {
			calls++
			old := got.Messages[1].Content[0].Content[0].Text
			if !strings.Contains(old, "history_omitted") {
				t.Fatal("old page not removed")
			}
			for _, m := range got.Messages {
				for _, b := range m.Content {
					if b.Type == llm.BlockToolResult {
						for _, c := range b.Content {
							if !json.Valid([]byte(noaadapter.StripRefTag(c.Text))) {
								t.Fatal("broken JSON/noa tag")
							}
						}
					}
				}
			}
			if noa && !strings.Contains(old, "noa-ref") {
				t.Fatal("lost noa reference")
			}
		}}, nil, 1)
		if _, _, _, e := provider.Complete(t.Context(), req); e != nil {
			t.Fatal(e)
		}
		for _, e := range provider.Stream(t.Context(), req) {
			if e != nil {
				t.Fatal(e)
			}
		}
		if calls != 2 {
			t.Fatal(calls)
		}
		after, _ := json.Marshal(req)
		if string(before) != string(after) {
			t.Fatal("raw or noa view changed")
		}
	}
}

func TestRetrievalAfterDefaultCompaction(t *testing.T) {
	req := retrievalFixture(100)
	before, _ := json.Marshal(req)
	calls := 0
	compact := compaction.New(compaction.Config{ContextWindow: 32000}, func(context.Context, []llm.Message) (string, error) {
		calls++
		return "历史证据按 node:1 的 /detail 重新读取；未展示不代表不存在。", nil
	})
	req.Messages = compact.Pre(t.Context(), req.Messages, 300000)
	if calls != 1 {
		t.Fatalf("summary calls=%d", calls)
	}
	result, err := FilterTaskAssetRequest(t.Context(), nil, 1, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("empty restore")
	}
	if calls != 1 {
		t.Fatal("extra summary request")
	}
	after, _ := json.Marshal(retrievalFixture(100))
	if string(before) != string(after) {
		t.Fatal("source fixture changed")
	}
}
