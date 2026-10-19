package server

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestFindingSuppressionRuleReadTimeoutDoesNotFailOpen(t *testing.T) {
	s, in, add := suppressionFixture(t)
	add("must be checked")
	version, err := s.m.pg.FindingSuppressionVersion(t.Context(), in.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := s.m.pg.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`LOCK TABLE finding_suppression_rules IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	check, err := s.findingRecorder().check(t.Context(), ctx, in, version)
	var infra *suppressionInfrastructureError
	if err == nil || !errors.As(err, &infra) || check.Status == "unchecked" {
		t.Fatalf("DB timeout became model fail-open: %+v %v", check, err)
	}
}

func TestFindingSuppressionProfileReadDeadline(t *testing.T) {
	s, _, _ := suppressionFixture(t)
	profile, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: "suppression deadline", Format: "openai", Model: "fixture", APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.DeleteProfile(profile)
	lock, err := s.m.pg.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.Exec(`LOCK TABLE llm_profiles IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = s.providerForProfileContext(ctx, profile)
	var infra *suppressionInfrastructureError
	if !errors.As(err, &infra) || time.Since(start) > time.Second {
		t.Fatalf("profile read lost error/deadline: %v", err)
	}
}

type suppressionTransportModel struct {
	stream, complete int
	text             string
}

func (p *suppressionTransportModel) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.stream++
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: p.text}, nil)
	}
}
func (p *suppressionTransportModel) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.complete++
	return llm.Message{Role: "assistant", Content: []llm.ContentBlock{{Type: llm.BlockText, Text: p.text}}}, "end_turn", llm.Usage{}, nil
}
func TestFindingSuppressionModelTransportAndOutputLimit(t *testing.T) {
	s, in, add := suppressionFixture(t)
	add("scope")
	p := &suppressionTransportModel{text: `{"decision":"allow","reason":"different target"}`}
	s.llmOn, s.llmProv = true, p
	for _, stream := range []bool{false, true} {
		s.llmCfg = agent.Config{Stream: stream}
		out, err := s.findingRecorder().Record(t.Context(), in, nil)
		if err != nil || out.SuppressionCheck.Status != "allowed" {
			t.Fatal(out, err)
		}
	}
	if p.stream != 1 || p.complete != 1 {
		t.Fatalf("ignored transport: %+v", p)
	}
	p.text = strings.Repeat("x", 32769)
	for _, stream := range []bool{false, true} {
		s.llmCfg.Stream = stream
		out, err := s.findingRecorder().Record(t.Context(), in, nil)
		if err != nil || out.SuppressionCheck.Status != "unchecked" {
			t.Fatal("unbounded output not diagnosed", out, err)
		}
	}
}

func TestFindingSuppressionPermissionsChangeAfterReview(t *testing.T) {
	for _, role := range []string{"planner", "mainagent", "worker"} {
		t.Run(role, func(t *testing.T) {
			s, in, add := suppressionFixture(t)
			add("scope")
			assets := s.m.pg.Assets()
			id, err := assets.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("commit-%d.test", in.TaskID), TaskID: in.TaskID})
			if err != nil {
				t.Fatal(err)
			}
			in.AssetIDs = []int64{id}
			in.AccessRole = role
			es := s.m.pg.Exploration(in.ExplorationID)
			in.IntentID, err = es.AddIntent(map[string]any{"summary": "fixture"}, 1, in.AssetIDs, "planner")
			if err != nil {
				t.Fatal(err)
			}
			if role == "worker" {
				if err := es.SetNodeState(in.IntentID, "running"); err != nil {
					t.Fatal(err)
				}
			}
			in.Revalidate = func(context.Context) error { return assets.ValidateTaskAssetsApproved(in.TaskID, in.AssetIDs) }
			r := s.findingRecorder()
			r.judge = func(context.Context, db.RecordFindingInput, []db.FindingSuppressionRule) (suppressionVerdict, error) {
				return suppressionVerdict{Decision: "allow", Reason: "different finding"}, nil
			}
			realRecord := r.record
			r.record = func(ctx context.Context, input db.RecordFindingInput, refs []db.TrafficRef) (*db.RecordedFinding, error) {
				if err := assets.RevokeTaskAssets(in.TaskID, in.AssetIDs, "user", "after early recheck"); err != nil {
					return nil, err
				}
				return realRecord(ctx, input, refs)
			}
			if out, err := r.Record(t.Context(), in, nil); err == nil || out != nil || !strings.Contains(err.Error(), "授权") {
				t.Fatal("final permission race", out, err)
			}
			var count int
			if err := s.m.pg.QueryRow(`SELECT count(*) FROM findings WHERE task_id=$1`, in.TaskID).Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
		})
	}
}

func TestFindingSuppressionPendingAndStoppedWorker(t *testing.T) {
	s, in, add := suppressionFixture(t)
	add("scope")
	if err := s.m.pg.SetAssetApprovalTemplate(in.TaskID, "explicit_targets"); err != nil {
		t.Fatal(err)
	}
	id, err := s.m.pg.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("pending-%d.test", in.TaskID), TaskID: in.TaskID, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	// Test explicit role policy independent of the configured autoapproval template.
	if _, err = s.m.pg.Exec(`UPDATE task_asset_links SET approval_state='pending' WHERE task_id=$1 AND asset_id=$2`, in.TaskID, id); err != nil {
		t.Fatal(err)
	}
	in.AssetIDs = []int64{id}
	es := s.m.pg.Exploration(in.ExplorationID)
	in.IntentID, err = es.AddIntent(map[string]any{"summary": "pending fixture"}, 1, in.AssetIDs, "planner")
	if err != nil {
		t.Fatal(err)
	}
	r := s.findingRecorder()
	r.judge = func(context.Context, db.RecordFindingInput, []db.FindingSuppressionRule) (suppressionVerdict, error) {
		return suppressionVerdict{Decision: "allow", Reason: "different finding"}, nil
	}
	for _, role := range []string{"planner", "mainagent", "worker"} {
		in.AccessRole = role
		if role == "worker" {
			if err := es.SetNodeState(in.IntentID, "running"); err != nil {
				t.Fatal(err)
			}
		}
		out, err := r.Record(t.Context(), in, nil)
		if role == "planner" {
			if err == nil {
				t.Fatal("planner pending bypass")
			}
		} else if err != nil || out == nil {
			t.Fatal(role, out, err)
		}
	}
	if err := es.SetNodeState(in.IntentID, "done"); err != nil {
		t.Fatal(err)
	}
	if out, err := r.Record(t.Context(), in, nil); err == nil || out != nil {
		t.Fatal("stopped worker wrote back", out, err)
	}
}

func TestFindingSuppressionToolMissingAssetStoreCannotSkipFinalPermission(t *testing.T) {
	s, in, _ := suppressionFixture(t)
	assets := s.m.pg.Assets()
	id, err := assets.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("missing-store-%d.test", in.TaskID), TaskID: in.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if err := assets.RevokeTaskAssets(in.TaskID, []int64{id}, "user", ""); err != nil {
		t.Fatal(err)
	}
	ts := agent.NewToolSet(s.m.pg.Exploration(in.ExplorationID), "mainagent")
	ts.SetTaskID(in.TaskID)
	ts.SetFindingRecorder(s.findingRecorder()) // Deliberately no SetAssetStore.
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: in.TaskID, AgentKey: "mainagent"})
	report := workflowTool(t, ts.MainAgentTools(), "report_finding")
	message := workflowCall(t, ctx, report, map[string]any{"vulnclass": "IDOR", "severity": "high", "summary": "test", "asset_ids": []int64{id}, "AccessRole": "mainagent"}, true)
	if !strings.Contains(message, "授权") {
		t.Fatal(message)
	}
}
