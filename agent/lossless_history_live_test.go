//go:build liveeval

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/db"
)

type historyLiveMeter struct {
	llm.Provider
	Requests, Input, Uncached, Output int
	Errors, UnknownUsageRequests      int
	identity                          string
}

func historyLiveAnswer(text, golden string) (validJSON, proof, constraints bool) {
	var answer map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &answer) != nil || len(answer) != 3 {
		return false, false, false
	}
	var value, receipt, action string
	if json.Unmarshal(answer["proof"], &value) != nil || json.Unmarshal(answer["committed_receipt"], &receipt) != nil || json.Unmarshal(answer["next_action"], &action) != nil {
		return false, false, false
	}
	return true, value == golden, receipt == "COMMITTED-42" && action == "review_only"
}

func TestLosslessLiveAnswerValidation(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{`{"proof":"EXACT","committed_receipt":"COMMITTED-42","next_action":"review_only"}`, true},
		{`{"proof":"not EXACT","committed_receipt":"COMMITTED-42","next_action":"review_only"}`, false},
		{`{"proof":"EXACT","committed_receipt":"COMMITTED-42","next_action":"do not review_only"}`, false},
		{`{"proof":"EXACT","committed_receipt":"COMMITTED-42","next_action":"review_only"`, false},
	} {
		valid, proof, constraints := historyLiveAnswer(tc.text, "EXACT")
		if (valid && proof && constraints) != tc.want {
			t.Fatal("incorrect evidence acceptance")
		}
	}
}

func (p *historyLiveMeter) Complete(ctx context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.Requests++
	// Isolate cross-run prefix caching, including the SDK's summary calls.
	// The same tag remains stable within a run so its normal cache still works.
	r.System = append([]string{"Benchmark trace ID (not evidence): " + p.identity}, r.System...)
	m, s, u, e := p.Provider.Complete(ctx, r)
	if e != nil {
		p.Errors++
		if u.InputTokens == 0 && u.OutputTokens == 0 {
			p.UnknownUsageRequests++
		}
	}
	p.Input += u.InputTokens
	p.Uncached += max(0, u.InputTokens-u.CacheReadTokens)
	p.Output += u.OutputTokens
	return m, s, u, e
}
func (p *historyLiveMeter) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	panic("non-streaming fixture")
}

// Real model continuation over seeded, durable long history. No shell command
// or production task is executed. All evidence lives in an isolated test DB.
func TestLiveLosslessHistoryComparison(t *testing.T) {
	path := os.Getenv("ARTEX_LIVE_CONFIG")
	if path == "" {
		t.Skip("requires production model config")
	}
	dsn := os.Getenv("SERIES_PG_DSN")
	if dsn == "" {
		t.Fatal("requires explicit isolated SERIES_PG_DSN; never use the application database by default")
	}
	var cfg struct {
		Format, Model string
		BaseURL       string `json:"base_url"`
		APIKey        string `json:"api_key"`
		Proxy         string
		Thinking      string `json:"thinking_type"`
		Effort        string `json:"reasoning_effort"`
	}
	raw, e := os.ReadFile(path)
	if e != nil || json.Unmarshal(raw, &cfg) != nil {
		t.Fatal("invalid configuration")
	}
	model := ConfigFrom(cfg.Format, cfg.Model, cfg.BaseURL, cfg.APIKey, cfg.Proxy)
	model.ThinkingType = cfg.Thinking
	model.ReasoningEffort = cfg.Effort
	out := os.Getenv("ARTEX_LIVE_OUT")
	if out == "" {
		t.Fatal("ARTEX_LIVE_OUT required")
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal("isolated test database unavailable")
	}
	defer d.Close()
	var metrics []map[string]any
	maxOutput := 32768 // observed request limit in task 160; same in both arms
	repeats := 2
	if raw := os.Getenv("ARTEX_LIVE_REPEATS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 4 {
			t.Fatal("repeats must be 1..4")
		}
		repeats = n
	}
	for repeat := 0; repeat < repeats; repeat++ {
		for _, scenario := range []string{"large_result", "stage_resume", "row_value", "full_url"} {
			variants := []string{"before", "after"}
			if repeat%2 == 1 {
				variants = []string{"after", "before"}
			}
			for _, variant := range variants {
				name := fmt.Sprintf("%s-%d-%s", scenario, repeat, variant)
				t.Run(name, func(t *testing.T) {
					task, err := d.CreateTask("history live fixture", "synthetic evidence only", nil, 0, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer d.DeleteTask(task.ID)
					es := d.Exploration(task.ExplorationID)
					intent, err := es.AddIntent(map[string]any{"summary": "history live fixture"}, 1, nil, "test")
					if err != nil {
						t.Fatal(err)
					}
					ts := NewToolSet(es, "worker")
					ts.SetTaskID(task.ID)
					ts.workerExecution = true
					ctx := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, ExplorationID: task.ExplorationID, IntentID: intent, AgentKey: "worker"})
					ctx = withExecutionCheckpoint(ctx, ts, "worker", intent)
					ctx = WithContextAccess(ctx, ContextAccess{Local: []string{"activity_detail", "activity"}})
					provider, err := model.NewProvider()
					if err != nil {
						t.Fatal("provider unavailable")
					}
					meter := &historyLiveMeter{Provider: provider, identity: fmt.Sprintf("%x", sha256.Sum256([]byte(os.Getenv("ARTEX_LIVE_CAMPAIGN")+"/"+name)))[:24]}
					count := 6
					if scenario == "stage_resume" {
						count = 20
					}
					seed := fmt.Sprintf("%s/%d/%s", os.Getenv("ARTEX_LIVE_CAMPAIGN"), repeat, scenario)
					golden := fmt.Sprintf("PROOF-%x", sha256.Sum256([]byte(seed)))[:30]
					targetValue := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/1/121", seed))))
					if scenario == "row_value" {
						golden = targetValue
					}
					if scenario == "full_url" {
						golden = "https://fixture.example.test/events/" + targetValue + "?record=121"
					}
					msgs := []llm.Message{llm.UserText("This is a synthetic evidence review. Do not execute commands or modify state. Preserve the receipt id COMMITTED-42: this write already completed and must never be repeated.")}
					var target int64
					for i := 0; i < count; i++ {
						var body strings.Builder
						for j := 0; j < 250; j++ {
							if scenario == "full_url" {
								fmt.Fprintf(&body, "https://fixture.example.test/events/%x?record=%03d [observed]\n", sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", seed, i, j))), j)
							} else {
								fmt.Fprintf(&body, "row=%03d sha=%x status=observed\n", j, sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", seed, i, j))))
							}
							if i == 1 && j == 120 && scenario != "row_value" && scenario != "full_url" {
								fmt.Fprintf(&body, "EXACT_PROOF=%s\n", golden)
							}
						}
						call := fmt.Sprintf("historic-%d", i)
						id, err := es.AppendActivity(db.Activity{NodeID: &intent, Worker: "worker", Kind: "tool_result", Tool: "Bash", ToolUseID: call, Detail: body.String()})
						if err != nil {
							t.Fatal(err)
						}
						if i == 1 {
							target = id
						}
						m := documentationCall(call, "Bash", `{"command":"synthetic fixture; already executed"}`)
						m.Content = append([]llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "I collected this synthetic result for later evidence review. The prior write COMMITTED-42 is complete."}}, m.Content...)
						msgs = append(msgs, m, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(call, body.String(), false)}})
					}
					// Give both variants the same retrieval route, never the answer.
					targetInstruction := "Obtain the EXACT_PROOF value only (without the EXACT_PROOF= label)"
					if scenario == "row_value" {
						targetInstruction = "Obtain the exact sha value for row=121"
					}
					if scenario == "full_url" {
						targetInstruction = "Obtain the complete URL (scheme, host, path and query) containing record=121"
					}
					msgs = append(msgs, llm.UserText(fmt.Sprintf("Return exactly one JSON object with three string fields: proof, committed_receipt, next_action. No Markdown fences or additional fields. proof must be only the requested exact value, without labels, explanations or a nested object. "+targetInstruction+" from the second historical result, durable ref activity:%d. Use context_search with types=[activity], intent_id="+fmt.Sprint(intent)+" and q to locate keywords, then context_get with the returned read. Direct context_get pagination is also available; field /detail, max_chars up to 24000. committed_receipt must preserve the previously completed write; next_action must be review_only. Never rerun Bash or repeat the write.", target)))
					get := ts.contextGet()
					search := ts.contextSearch()
					tools := []llm.ToolSchema{{Name: get.Name(), Description: get.Description(), InputSchema: get.InputSchema()}, {Name: search.Name(), Description: search.Description(), InputSchema: search.InputSchema()}}
					summaries := 0
					if scenario == "stage_resume" {
						sum := compaction.ProviderSummarizer(meter, 4000, true)
						c := compaction.New(compaction.Config{ContextWindow: 64000}, sum)
						msgs = c.Pre(ctx, msgs, 60000)
						summaries = meter.Requests
					}
					reads, invalid, repeated, success := 0, 0, 0, false
					seenReads := map[string]bool{}
					validJSON, proofCorrect, constraintsCorrect := false, false, false
					stopReason := ""
					start := time.Now()
					for round := 0; round < 10; round++ {
						req := llm.CompletionRequest{System: []string{"Fixture run " + seed + "/" + variant + ". Complete the evidence task accurately. Return final JSON only."}, Messages: llm.MessagesForAPI(msgs), Tools: tools, MaxTokens: maxOutput}
						if variant == "after" {
							req = compactExecutionLogHistory(req)
						}
						m, stop, _, err := meter.Complete(ctx, req)
						stopReason = stop
						if err != nil {
							t.Log("model request failed")
							break
						}
						msgs = append(msgs, m)
						calls := 0
						for _, b := range m.Content {
							if b.Type != llm.BlockToolUse {
								continue
							}
							calls++
							reads++
							var input any
							_ = json.Unmarshal(b.Input, &input)
							canonical, _ := json.Marshal(input)
							readKey := b.Name + ":" + string(canonical)
							if seenReads[readKey] {
								repeated++
							}
							seenReads[readKey] = true
							if b.Name != "context_get" && b.Name != "context_search" {
								invalid++
								msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(b.ID, "unauthorized tool", true)}})
								continue
							}
							tool := get
							if b.Name == "context_search" {
								tool = search
							}
							r, e := tool.Call(ctx, b.Input, nil)
							var result struct {
								Items []map[string]any `json:"items"`
							}
							bad := json.Unmarshal([]byte(r.Flatten()), &result) != nil
							for _, item := range result.Items {
								bad = bad || item["error"] != nil
							}
							if e != nil || r.IsError || bad {
								invalid++
							}
							msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(b.ID, r.Flatten(), r.IsError)}})
						}
						if calls == 0 {
							validJSON, proofCorrect, constraintsCorrect = historyLiveAnswer(m.Text(), golden)
							success = validJSON && proofCorrect && constraintsCorrect
							break
						}
					}
					trace, err := json.MarshalIndent(msgs, "", "  ")
					if err != nil {
						t.Fatal("cannot encode synthetic trace")
					}
					if err := os.WriteFile(filepath.Join(out, name+".messages.json"), trace, 0600); err != nil {
						t.Fatal(err)
					}
					metrics = append(metrics, map[string]any{"run": name, "success": success, "valid_json": validJSON, "proof_correct": proofCorrect, "constraints_correct": constraintsCorrect, "final_stop_reason": stopReason, "tool_calls": reads, "invalid_calls": invalid, "repeated_reads": repeated, "requests": meter.Requests, "model_errors": meter.Errors, "unknown_usage_requests": meter.UnknownUsageRequests, "input": meter.Input, "uncached": meter.Uncached, "output": meter.Output, "summaries": summaries, "elapsed_ms": time.Since(start).Milliseconds()})
					report, _ := json.MarshalIndent(map[string]any{"model": cfg.Model, "thinking": cfg.Thinking, "effort": cfg.Effort, "scope": "seeded synthetic history continuation, pressure window 64000; summary usage included", "max_output_tokens": maxOutput, "cache_isolation": "per-run system prefix including summary", "metrics": metrics}, "", "  ")
					if err := os.WriteFile(filepath.Join(out, "summary.json"), report, 0600); err != nil {
						t.Fatal(err)
					}
					t.Logf("success=%v calls=%d invalid=%d requests=%d uncached=%d", success, reads, invalid, meter.Requests, meter.Uncached)
					if !success {
						t.Error("exact evidence or task constraints were not satisfied")
					}
				})
			}
		}
	}
}
