package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/noaadapter"
)

func losslessFixture() string {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "row=%03d target=请求🙂-%04d status=observed original-policy=read-only\r\n", i, i)
		if i == 123 {
			b.WriteString("UNUSUAL_MIDDLE_EVIDENCE=exact-secret-123\r\n")
		}
		if i == 251 {
			b.WriteString("row=251 target=unexpected status=ERROR must-not-hide=yes\r\n")
		}
	}
	b.WriteString("tail without newline")
	return b.String()
}
func TestLosslessHistoryRoundtripAndFallback(t *testing.T) {
	original := losslessFixture()
	encoded := compactExecutionLog(original)
	var log losslessLog
	if encoded == original || json.Unmarshal([]byte(encoded), &log) != nil {
		t.Fatal("not compacted")
	}
	if restoreLog(log.Segments) != original {
		t.Fatal("lost exact evidence")
	}
	if !strings.Contains(encoded, "UNUSUAL_MIDDLE_EVIDENCE=exact-secret-123") || !strings.Contains(encoded, "status=ERROR") {
		t.Fatal("exception lost")
	}
	if len(encoded)*100 > len(original)*85 {
		t.Fatal("insufficient savings")
	}
	if compactExecutionLog(encoded) != encoded {
		t.Fatal("non-idempotent")
	}
	for _, s := range []string{"tiny", `{"body":"` + strings.Repeat("row=123 status=ok ", 3000) + `"}`, strings.Repeat("x", 4<<20) + "x", strings.Repeat("x", 13000) + string([]byte{0xff})} {
		if compactExecutionLog(s) != s {
			t.Fatal("unsafe input changed")
		}
	}
	t.Logf("bytes: %d -> %d", len(original), len(encoded))
}
func TestLosslessHistoryProjectionAndNOA(t *testing.T) {
	body := losslessFixture()
	for _, name := range []string{"Bash", "Read", "context_get"} {
		req := llm.CompletionRequest{MaxTokens: 32768, Messages: []llm.Message{documentationCall("a", name, `{}`), {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("a", body, false)}}}}
		before, _ := json.Marshal(req)
		out := compactExecutionLogHistory(req)
		after, _ := json.Marshal(req)
		if string(before) != string(after) {
			t.Fatal("audit mutated")
		}
		if (out.Messages[1].Content[0].Content[0].Text != body) != (name == "Bash") {
			t.Fatal("wrong tool modified", name)
		}
		req.Messages[1].Content[0].IsError = true
		if !reflect.DeepEqual(req, compactExecutionLogHistory(req)) {
			t.Fatal("error altered")
		}
	}
	for _, noa := range []bool{false, true} {
		req := llm.CompletionRequest{MaxTokens: 32768, Messages: []llm.Message{documentationCall("a", "Bash", `{}`), {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("a", body, false)}}}}
		if noa {
			opts := agentcore.Options{}
			enableNoa(&opts, func() bool { return true }, t.TempDir(), "lossless", nil)
			req.Messages = opts.Compactor.(harness.ContextView).View(t.Context(), req.Messages)
		}
		before, _ := json.Marshal(req)
		provider := withAssetContext(retrievalProbeProvider{check: func(r llm.CompletionRequest) {
			text := r.Messages[1].Content[0].Content[0].Text
			var log losslessLog
			if json.Unmarshal([]byte(noaadapter.StripRefTag(text)), &log) != nil || restoreLog(log.Segments) != body {
				t.Fatal("provider lost evidence")
			}
			if noa && !strings.Contains(text, "noa-ref") {
				t.Fatal("NOA reference lost")
			}
		}}, nil, 1)
		if _, _, _, err := provider.Complete(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		for _, err := range provider.Stream(t.Context(), req) {
			if err != nil {
				t.Fatal(err)
			}
		}
		after, _ := json.Marshal(req)
		if string(before) != string(after) {
			t.Fatal("provider mutated audit")
		}
	}
}
func FuzzLosslessLogRoundtrip(f *testing.F) {
	f.Add(losslessFixture())
	f.Add("row=1 status=ok\n")
	f.Add(strings.Repeat("row=123 status=ok\n", 1000))
	f.Fuzz(func(t *testing.T, s string) {
		got := compactExecutionLog(s)
		if got == s {
			return
		}
		var log losslessLog
		if json.Unmarshal([]byte(got), &log) != nil || restoreLog(log.Segments) != s {
			t.Fatal("not lossless")
		}
	})
}

func TestLosslessHistoryPreservesDuplicateFactoring(t *testing.T) {
	body := losslessFixture()
	req := llm.CompletionRequest{MaxTokens: 32768}
	for i := 0; i < 4; i++ {
		id := fmt.Sprint(i)
		req.Messages = append(req.Messages, documentationCall(id, "Bash", `{}`), llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, body, false)}})
	}
	out := compactExecutionLogHistory(deduplicatePlainHistory(req))
	duplicates, complete := 0, 0
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.Type != llm.BlockToolResult {
				continue
			}
			text := b.Content[0].Text
			if strings.Contains(text, `"history_duplicate":true`) {
				duplicates++
				continue
			}
			var log losslessLog
			if json.Unmarshal([]byte(text), &log) == nil && restoreLog(log.Segments) == body {
				complete++
			}
		}
	}
	if duplicates != 2 || complete != 2 {
		t.Fatalf("duplicates=%d complete=%d", duplicates, complete)
	}
}

func TestLosslessHistoryGenericSpans(t *testing.T) {
	var url strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&url, "https://fixture.example.test/events/entry-%04d?record=%03d [observed]\r\n", i, i)
	}
	for _, body := range []string{url.String(), strings.Repeat("中文🙂 repeated diagnostic without assignments\r\n", 1000)} {
		compact := compactExecutionLog(body)
		var log losslessLog
		if compact == body || json.Unmarshal([]byte(compact), &log) != nil || restoreLog(log.Segments) != body {
			t.Fatal("generic factorization not lossless")
		}
	}
}

func TestLosslessHistoryLowOutputBudgetKeepsOriginal(t *testing.T) {
	req := llm.CompletionRequest{MaxTokens: 4000, Messages: []llm.Message{documentationCall("a", "Bash", `{}`), {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("a", losslessFixture(), false)}}}}
	for _, budget := range []int{0, 4000, 8191} {
		req.MaxTokens = budget
		if !reflect.DeepEqual(req, compactExecutionLogHistory(req)) {
			t.Fatal("tight or unknown budget request was transformed")
		}
	}
	req.MaxTokens = 32768
	if reflect.DeepEqual(req, compactExecutionLogHistory(req)) {
		t.Fatal("normal-budget request did not transform")
	}
}
