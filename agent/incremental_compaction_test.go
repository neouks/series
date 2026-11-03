package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/db"
)

type summaryTestProvider struct {
	calls  int
	before func()
	omit   bool
}

func (p *summaryTestProvider) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	var input struct {
		Members []db.GraphCandidate `json:"members"`
	}
	if err := json.Unmarshal([]byte(req.Messages[0].Content[0].Text), &input); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	if len(input.Members) > 32 || req.MaxTokens != 1500 || utf8.RuneCountInString(req.Messages[0].Content[0].Text) > 24000 {
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("summary exceeded budget")
	}
	if p.before != nil {
		p.before()
	}
	ids := []int64{}
	for _, m := range input.Members {
		ids = append(ids, m.ID)
	}
	if p.omit {
		ids = ids[:1]
	}
	raw, _ := json.Marshal(graphSummaryOutput{Items: []graphSummaryItem{{Text: "Observed records require source verification", Refs: ids, Confidence: "inferred"}}})
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock(string(raw))}}, "end_turn", llm.Usage{InputTokens: 100, OutputTokens: 30}, nil
}
func (p *summaryTestProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		m, _, u, e := p.Complete(ctx, req)
		if e != nil {
			yield(llm.StreamEvent{}, e)
			return
		}
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: m.Text()}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: u}, nil)
	}
}

func TestIncrementalSummaryPublicationAndSourceChange(t *testing.T) {
	for _, mode := range []string{"success", "unrelated", "changed", "omitted", "manual", "paused", "pending", "revoked", "escaped"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			defer d.Close()
			task, err := d.CreateTaskWithOptions("summary publication", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
			if err != nil {
				t.Fatal(err)
			}
			defer d.DeleteTask(task.ID)
			store := d.Exploration(task.ExplorationID)
			var assets []int64
			if mode == "pending" || mode == "revoked" {
				id, e := d.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("summary-%d.test", task.ID), TaskID: task.ID, AgentDiscovered: true})
				if e != nil {
					t.Fatal(e)
				}
				assets = []int64{id}
				if mode != "pending" {
					if e = d.Assets().ApproveTaskAssets(task.ID, assets, "test", ""); e != nil {
						t.Fatal(e)
					}
				}
			}
			root, err := store.AddNode(db.KindIntent, map[string]any{"summary": "parent"}, 0, "done", "test", nil)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			sourceCount := 3
			summary := "source"
			if mode == "escaped" {
				sourceCount = 32
				summary = strings.Repeat("\x01", 600)
			}
			for range sourceCount {
				id, e := store.AddNode(db.KindFact, map[string]any{"summary": summary, "confidence": "observed"}, 0, "confirmed", "test", assets)
				if e != nil {
					t.Fatal(e)
				}
				ids = append(ids, id)
				if e = store.Link(root, db.RelYields, id); e != nil {
					t.Fatal(e)
				}
			}
			for i := 0; i < 30; i++ {
				ready, e := store.BootstrapGraphProjection(t.Context())
				if e != nil {
					t.Fatal(e)
				}
				if !ready {
					continue
				}
				n, e := store.MaintainGraphPage(t.Context())
				if e != nil {
					t.Fatal(e)
				}
				if n == 0 {
					break
				}
			}
			if _, err = d.Exec(`UPDATE graph_node_projection SET cold_since_at=now()-interval '11 minutes' WHERE exploration_id=$1 AND NOT hot`, store.ID()); err != nil {
				t.Fatal(err)
			}
			candidates, err := store.GraphCandidates(t.Context(), 128)
			if err != nil {
				t.Fatal(err)
			}
			members := []db.GraphCandidate{}
			for _, n := range candidates {
				for _, id := range ids {
					if n.ID == id {
						members = append(members, n)
					}
				}
			}
			if len(members) != len(ids) {
				t.Fatal("missing candidates", members)
			}
			p := &summaryTestProvider{omit: mode == "omitted"}
			if mode == "revoked" {
				p.before = func() {
					if _, e := d.Assets().DetachAssetFromTask(task.ID, assets[0]); e != nil {
						t.Fatal(e)
					}
				}
			}
			if mode == "unrelated" {
				p.before = func() {
					if _, e := store.AddNode(db.KindFact, map[string]any{"summary": "unrelated new evidence"}, 0, "confirmed", "test", nil); e != nil {
						t.Fatal(e)
					}
				}
			}
			if mode == "changed" {
				p.before = func() {
					if _, e := d.Exec(`UPDATE exploration_nodes SET payload=payload||'{"summary":"changed during generation"}'::jsonb WHERE id=$1`, ids[0]); e != nil {
						t.Fatal(e)
					}
				}
			}
			if mode == "manual" {
				if _, err = d.Exec(`UPDATE tasks SET execution_mode='manual' WHERE id=$1`, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "paused" {
				if _, err = d.Exec(`UPDATE tasks SET paused=true,status='paused' WHERE id=$1`, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			c := NewCompactor(p, "")
			c.SetAssetStore(d.Assets())
			ctx, cancel := context.WithTimeout(t.Context(), time.Second*10)
			defer cancel()
			err = c.foldIncremental(ctx, store, members)
			digests, e := store.ActiveDigests()
			if e != nil {
				t.Fatal(e)
			}
			if mode == "success" || mode == "unrelated" || mode == "escaped" {
				if err != nil || len(digests) != 1 {
					t.Fatal("summary not published", err, len(digests))
				}
			} else {
				if err == nil || len(digests) != 0 {
					t.Fatal("invalid generation published", mode, err, len(digests))
				}
			}
			if (mode == "manual" || mode == "paused" || mode == "pending") && p.calls != 0 {
				t.Fatal("inactive task called summary model")
			}
		})
	}
}
