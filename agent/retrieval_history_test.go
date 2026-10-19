package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func retrievalFixture(pages int) llm.CompletionRequest {
	req := llm.CompletionRequest{System: []string{"system"}}
	for i := 0; i < pages; i++ {
		id := fmt.Sprintf("read-%d", i)
		in := fmt.Sprintf(`{"items":[{"ref":"node:1","field":"/detail","offset":%d}]}`, i*4000)
		body, _ := json.Marshal(map[string]any{"items": []any{map[string]any{"ref": "node:1", "node_id": 1, "payload": map[string]any{"value": strings.Repeat("中证据", 1333), "offset": i * 4000, "next_offset": (i + 1) * 4000}, "next": map[string]any{"items": []any{map[string]any{"ref": "node:1", "field": "/detail", "offset": (i + 1) * 4000}}}}}})
		req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "context_get", Input: json.RawMessage(in)}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, string(body), false)}})
	}
	return req
}
func TestRetrievalHistoryWindowAndAudit(t *testing.T) {
	for _, n := range []int{2, 10, 100} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			req := retrievalFixture(n)
			before, _ := json.Marshal(req)
			got := boundRetrievalHistory(req)
			again := boundRetrievalHistory(req)
			if !reflect.DeepEqual(got, again) {
				t.Fatal("nondeterministic projection")
			}
			after, _ := json.Marshal(req)
			if string(before) != string(after) {
				t.Fatal("audit changed")
			}
			if len(req.Messages) != len(got.Messages) {
				t.Fatal("message structure changed")
			}
			dataRunes := 0
			for mi, m := range got.Messages {
				for bi, b := range m.Content {
					orig := req.Messages[mi].Content[bi]
					if b.Type == llm.BlockToolUse && !reflect.DeepEqual(b, orig) {
						t.Fatal("call changed")
					}
					if b.Type != llm.BlockToolResult {
						continue
					}
					if b.ToolUseID != orig.ToolUseID {
						t.Fatal("pair changed")
					}
					for _, c := range b.Content {
						if c.Type == llm.BlockText {
							if !json.Valid([]byte(c.Text)) {
								t.Fatal("invalid JSON")
							}
							if c.Text != `{"history_omitted":true,"read_from_original_call":true}` {
								dataRunes += utf8.RuneCountInString(c.Text)
							}
						}
					}
				}
			}
			if dataRunes > retrievalBodyRunes {
				t.Fatalf("body budget=%d", dataRunes)
			}
			for _, s := range got.System {
				if strings.HasPrefix(s, retrievalDirectoryPrefix) && utf8.RuneCountInString(s) > retrievalDirectoryRunes {
					t.Fatal("directory budget")
				}
			}
			if n > 10 && !strings.Contains(got.Messages[len(got.Messages)-1].Content[0].Content[0].Text, "中证据") {
				t.Fatal("latest evidence gone")
			}
			raw, _ := json.Marshal(got)
			t.Logf("pages=%d before_bytes=%d before_chars=%d after_bytes=%d after_chars=%d body_chars=%d", n, len(before), utf8.RuneCount(before), len(raw), utf8.RuneCount(raw), dataRunes)
		})
	}
}
func TestRetrievalHistoryOversizedPartialAndDeferred(t *testing.T) {
	req := retrievalFixture(1)
	b := &req.Messages[1].Content[0]
	b.Content[0].Text = strings.ReplaceAll(b.Content[0].Text, strings.Repeat("中证据", 1333), strings.Repeat("大", 50000))
	original := req.Messages[0].Content[0]
	req.Messages[0].Content[0].Name = actool.ExecuteExtraToolName
	params, _ := json.Marshal(map[string]any{"tool_name": original.Name, "params": json.RawMessage(original.Input)})
	req.Messages[0].Content[0].Input = params
	got := boundRetrievalHistory(req)
	var result map[string]any
	if err := json.Unmarshal([]byte(got.Messages[1].Content[0].Content[0].Text), &result); err != nil {
		t.Fatal(err)
	}
	if result["history_omitted"] != true || result["next_is_after_original_page"] != true || result["reread"] == nil {
		t.Fatal(result)
	}
	// Failures and partial batch failures never vanish behind a success marker.
	for _, failure := range []string{`{"items":[{"ref":"node:1","error":"denied"}]}`, `{"context_unavailable":true}`} {
		req.Messages[1].Content[0].Content[0].Text = failure
		got = boundRetrievalHistory(req)
		if got.Messages[1].Content[0].Content[0].Text != failure {
			t.Fatal("lost diagnostic")
		}
	}
}
func TestRetrievalDirectoryBoundedAndNoSecrets(t *testing.T) {
	req := retrievalFixture(100)
	for i := 0; i < 100; i++ {
		req.Messages[i*2].Content[0].Input = json.RawMessage(fmt.Sprintf(`{"items":[{"ref":"node:%d","field":"/auth"}]}`, i+1))
		req.Messages[i*2+1].Content[0].Content[0].Text = fmt.Sprintf(`{"items":[{"ref":"node:%d","payload":{"value":%q}}]}`, i+1, strings.Repeat("PRIVATE_CREDENTIAL", 500))
	}
	got := boundRetrievalHistory(req)
	for _, s := range got.System {
		if strings.HasPrefix(s, retrievalDirectoryPrefix) {
			if strings.Contains(s, "PRIVATE_CREDENTIAL") || utf8.RuneCountInString(s) > 4000 || !strings.Contains(s, `"truncated":true`) {
				t.Fatal(s)
			}
		}
	}
}
func TestRetrievalReReadBecomesLatest(t *testing.T) {
	req := retrievalFixture(20)
	req.Messages = append(req.Messages, req.Messages[0], req.Messages[1])
	req.Messages[len(req.Messages)-2].Content = append([]llm.ContentBlock(nil), req.Messages[0].Content...)
	req.Messages[len(req.Messages)-2].Content[0].ID = "reread"
	req.Messages[len(req.Messages)-1].Content = append([]llm.ContentBlock(nil), req.Messages[1].Content...)
	req.Messages[len(req.Messages)-1].Content[0].ToolUseID = "reread"
	got := boundRetrievalHistory(deduplicateToolHistory(req))
	if !strings.Contains(got.Messages[len(got.Messages)-1].Content[0].Content[0].Text, "中证据") {
		t.Fatal("re-read evicted")
	}
}

func TestRetrievalNonTextAndWriteResultsUntouched(t *testing.T) {
	req := retrievalFixture(20)
	for _, name := range []string{"record_fact", "report_finding", "check_target_access", "list_task_assets"} {
		id := "write-" + name
		req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(`{}`)}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, `{"receipt":"keep me"}`, false)}})
	}
	req.Messages[1].Content[0].Content = append(req.Messages[1].Content[0].Content, llm.ContentBlock{Type: llm.BlockThinking, Thinking: "non text block"})
	got := boundRetrievalHistory(req)
	if !reflect.DeepEqual(req.Messages[len(req.Messages)-8:], got.Messages[len(got.Messages)-8:]) {
		t.Fatal("write or approval changed")
	}
	if got.Messages[1].Content[0].Content[1].Thinking != "non text block" {
		t.Fatal("lost nontext")
	}
}

func TestRetrievalDirectoryPreservesWindows(t *testing.T) {
	req := retrievalFixture(20)
	got := boundRetrievalHistory(req)
	found := false
	for _, s := range got.System {
		if strings.HasPrefix(s, retrievalDirectoryPrefix) {
			found = true
			if !strings.Contains(s, `"field":"/detail"`) || !strings.Contains(s, `"offset":`) || !strings.Contains(s, `"next_offset":`) {
				t.Fatal("window lost", s)
			}
		}
	}
	if !found {
		t.Fatal("missing history directory")
	}
}

func TestRetrievalPartialOverviewKeepsDiagnostics(t *testing.T) {
	req := retrievalFixture(20)
	for i, body := range []string{
		`{"partial":true,"unavailable":{"recent_facts":"database query failed"},"task":{"goal":"g"}}`,
		`{"asset_approval_error":"approval query failed","task":{"goal":"g"}}`,
		`{"related_tasks":[{"source_task_id":3,"unavailable":{"fact":"source query failed"}}],"task":{"goal":"g"}}`,
		`{"task":{"goal":"g"},"recent_facts":[]}`,
	} {
		id := fmt.Sprintf("overview-%d", i)
		req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "graph_overview", Input: json.RawMessage(`{}`)}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, body, false)}})
	}
	// Enough later data forces both old overviews outside the body window.
	req.Messages = append(req.Messages, retrievalFixture(20).Messages...)
	got := boundRetrievalHistory(deduplicateToolHistory(req))
	for _, i := range []int{41, 43, 45} {
		if !reflect.DeepEqual(req.Messages[i], got.Messages[i]) {
			t.Fatal("lost partial overview diagnostics")
		}
	}
}

func TestRetrievalBatchKeepsDistinctFields(t *testing.T) {
	call := llm.ContentBlock{Name: "context_get", ID: "batch", Input: json.RawMessage(`{"items":[{"ref":"node:1","field":"/detail","offset":30},{"ref":"node:1","field":"/evidence","offset":400}]}`)}
	value := map[string]any{"items": []any{
		map[string]any{"ref": "node:1", "payload": map[string]any{"value": "detail"}},
		map[string]any{"ref": "node:1", "payload": map[string]any{"value": "evidence"}},
	}}
	index := retrievalIndex(value, call)
	raw, _ := json.Marshal(index)
	if !strings.Contains(string(raw), `"field":"/detail","offset":30`) || !strings.Contains(string(raw), `"field":"/evidence","offset":400`) {
		t.Fatal("batch windows collapsed", string(raw))
	}
	asset := llm.ContentBlock{Name: "asset_get", ID: "asset", Input: json.RawMessage(`{"ids":[3],"fields":["fingerprint"],"field":"/fingerprint","index":20}`)}
	index = retrievalIndex(map[string]any{"assets": []any{map[string]any{"asset_id": 3, "details": map[string]any{"next_index": 40}}}}, asset)
	raw, _ = json.Marshal(index)
	if !strings.Contains(string(raw), `"next_index":40`) || !strings.Contains(string(raw), `"fields":["fingerprint"]`) {
		t.Fatal("asset continuation lost", string(raw))
	}
}

func TestRetrievalParallelReadKinds(t *testing.T) {
	text := strings.Repeat("证据", 9000)
	cases := []struct {
		name, args string
		value      any
	}{
		{"context_search", `{"types":["fact"],"q":"证据"}`, map[string]any{"items": []any{map[string]any{"ref": "node:1", "snippet": text, "match_field": "/detail", "match_offset": 800}}}},
		{"context_get", `{"items":[{"ref":"node:1","field":"/detail","offset":740}]}`, map[string]any{"items": []any{map[string]any{"ref": "node:1", "payload": map[string]any{"value": text}}}}},
		{"asset_search", `{"dsl":"type=host"}`, map[string]any{"items": []any{map[string]any{"id": 3, "description": text}}}},
		{"asset_get", `{"ids":[3]}`, map[string]any{"assets": []any{map[string]any{"asset_id": 3, "details": map[string]any{"description": text}}}}},
		{"traffic_search", `{"host":"example.test"}`, map[string]any{"items": []any{map[string]any{"id": "traffic-1", "summary": text}}}},
		{"traffic_get", `{"id":"traffic-1","part":"response_body","offset":0}`, map[string]any{"id": "traffic-1", "parts": []any{map[string]any{"part": "response_body", "text": text, "next_offset": 8192}}}},
		{"graph_overview", `{}`, map[string]any{"task": map[string]any{"goal": "goal"}, "recent_facts": []any{map[string]any{"id": 1, "summary": text}}}},
	}
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleAssistant}, {Role: llm.RoleUser}}}
	for _, c := range cases {
		raw, _ := json.Marshal(c.value)
		req.Messages[0].Content = append(req.Messages[0].Content, llm.ContentBlock{Type: llm.BlockToolUse, Name: c.name, ID: c.name, Input: json.RawMessage(c.args)})
		req.Messages[1].Content = append(req.Messages[1].Content, llm.ToolResultText(c.name, string(raw), false))
	}
	got := boundRetrievalHistory(req)
	if !reflect.DeepEqual(got.Messages[0], req.Messages[0]) || len(got.Messages[1].Content) != len(cases) {
		t.Fatal("parallel call structure changed")
	}
	for i, b := range got.Messages[1].Content {
		if b.ToolUseID != cases[i].name || !json.Valid([]byte(b.Content[0].Text)) {
			t.Fatal("invalid result or pair")
		}
		if i < len(cases)-1 && !strings.Contains(b.Content[0].Text, "history_omitted") {
			t.Fatal("old read not compacted", cases[i].name)
		}
	}
	if !strings.Contains(got.Messages[1].Content[len(cases)-1].Content[0].Text, text) {
		t.Fatal("latest parallel read lost")
	}
}
