package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func suppressionFixture(t *testing.T) (*Server, db.RecordFindingInput, func(string) int64) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	task, err := m.pg.CreateTask("suppression fixture", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.pg.DeleteTask(task.ID) })
	s := &Server{m: m}
	in := db.RecordFindingInput{TaskID: task.ID, ExplorationID: task.ExplorationID, VulnClass: "IDOR", Name: "renamed finding", Summary: "same endpoint", Evidence: "new evidence", Worker: "test"}
	add := func(reason string) int64 {
		t.Helper()
		id, err := m.pg.AddFinding(task.ID, 0, "IDOR", "original name", "high", "same endpoint", "private proof", "worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		feedback, err := m.pg.DeleteFindingWithFeedback(id, reason)
		if err != nil {
			t.Fatal(err)
		}
		var ruleID int64
		if err = m.pg.QueryRow(`SELECT id FROM finding_suppression_rules WHERE feedback_id=$1`, feedback.ID).Scan(&ruleID); err != nil {
			t.Fatal(err)
		}
		return ruleID
	}
	return s, in, add
}

func TestFindingSuppressionDecisions(t *testing.T) {
	for _, kind := range []string{"allow", "deny", "unknown", "invalid", "failure", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			s, in, add := suppressionFixture(t)
			rule := add("")
			r := s.findingRecorder()
			calls := 0
			r.judge = func(ctx context.Context, _ db.RecordFindingInput, rules []db.FindingSuppressionRule) (suppressionVerdict, error) {
				calls++
				switch kind {
				case "failure":
					return suppressionVerdict{}, errors.New("credentials must not leak")
				case "timeout":
					<-ctx.Done()
					return suppressionVerdict{}, ctx.Err()
				case "deny":
					return suppressionVerdict{"deny", []int64{rule}, "same vulnerability even after rename"}, nil
				case "invalid":
					return suppressionVerdict{"deny", []int64{rule + 9999}, "forged rule"}, nil
				default:
					return suppressionVerdict{Decision: kind, Reason: "classification"}, nil
				}
			}
			if kind == "timeout" {
				r.budget = time.Millisecond * 20
			}
			out, err := r.Record(t.Context(), in, nil)
			if kind == "deny" {
				if err == nil || out != nil {
					t.Fatal("denial persisted finding")
				}
				var n int
				if err := s.m.pg.QueryRow(`SELECT count(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind='finding'`, in.ExplorationID).Scan(&n); err != nil || n != 0 {
					t.Fatalf("residue %d %v", n, err)
				}
				_, _ = r.Record(t.Context(), in, nil)
				if calls != 1 {
					t.Fatal("denied judgement not cached")
				}
				if err := s.m.pg.RevokeFindingSuppression(t.Context(), in.TaskID, rule); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Record(t.Context(), in, nil); err != nil {
					t.Fatal("revocation did not invalidate denial", err)
				}
				return
			}
			if err != nil || out == nil || out.SuppressionCheck == nil {
				t.Fatalf("record %v %v", out, err)
			}
			want := "unchecked"
			if kind == "allow" {
				want = "allowed"
			}
			if out.SuppressionCheck.Status != want {
				t.Fatal(out.SuppressionCheck)
			}
			if strings.Contains(out.SuppressionCheck.Reason, "credentials") {
				t.Fatal("model error leaked")
			}
			if want == "unchecked" {
				var n int
				if err := s.m.pg.QueryRow(`SELECT count(*) FROM activity WHERE exploration_id=$1 AND summary='禁报校验未完成，按用户策略允许上报'`, in.ExplorationID).Scan(&n); err != nil || n != 1 {
					t.Fatalf("missing failure audit %d %v", n, err)
				}
			}
		})
	}
}

func TestFindingSuppressionRaceAndIsolation(t *testing.T) {
	s, in, add := suppressionFixture(t)
	r := s.findingRecorder()
	calls := 0
	r.judge = func(ctx context.Context, _ db.RecordFindingInput, rules []db.FindingSuppressionRule) (suppressionVerdict, error) {
		calls++
		if calls == 1 {
			add("禁止同类 IDOR")
			return suppressionVerdict{Decision: "allow", Reason: "old snapshot"}, nil
		}
		return suppressionVerdict{"deny", []int64{rules[0].ID}, "new rule"}, nil
	}
	// No rules => no model call.
	first, err := r.Record(t.Context(), in, nil)
	if err != nil || calls != 0 {
		t.Fatalf("no rules %v", err)
	}
	s.m.pg.DeleteFinding(first.FindingID)
	add("原接口不再上报")
	if _, err = r.Record(t.Context(), in, nil); err == nil || calls != 2 {
		t.Fatalf("rule race bypass: calls=%d err=%v", calls, err)
	}
	other, err := s.m.pg.CreateTask("other", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.m.pg.DeleteTask(other.ID)
	in.TaskID, in.ExplorationID = other.ID, other.ExplorationID
	if _, err = r.Record(t.Context(), in, nil); err != nil || calls != 2 {
		t.Fatal("cross task rule leaked", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = r.Record(ctx, in, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel failed open", err)
	}
}

func TestFindingSuppressionFinalTransactionRace(t *testing.T) {
	s, in, add := suppressionFixture(t)
	r := s.findingRecorder()
	realRecord := r.record
	once := false
	r.record = func(ctx context.Context, in db.RecordFindingInput, refs []db.TrafficRef) (*db.RecordedFinding, error) {
		if !once {
			once = true
			add("stop")
		}
		return realRecord(ctx, in, refs)
	}
	r.judge = func(_ context.Context, _ db.RecordFindingInput, rules []db.FindingSuppressionRule) (suppressionVerdict, error) {
		return suppressionVerdict{"deny", []int64{rules[0].ID}, "stop"}, nil
	}
	if _, err := r.Record(t.Context(), in, nil); err == nil {
		t.Fatal("deletion between review and commit bypassed")
	}
}

func TestFindingSuppressionPaginationHTTP(t *testing.T) {
	s, in, add := suppressionFixture(t)
	rule := add("reason")
	add("")
	req := httptest.NewRequest("GET", "/?limit=1", nil)
	req.SetPathValue("id", fmt.Sprint(in.TaskID))
	w := httptest.NewRecorder()
	s.listFindingSuppression(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"has_more":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, tid := range []int64{in.TaskID + 100000, in.TaskID} {
		req := httptest.NewRequest("POST", "/", nil)
		req.SetPathValue("id", fmt.Sprint(tid))
		req.SetPathValue("rule_id", fmt.Sprint(rule))
		w := httptest.NewRecorder()
		s.revokeFindingSuppression(w, req)
		want := 200
		if tid != in.TaskID {
			want = 404
		}
		if w.Code != want {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestFindingSuppressionAllPagesAndBudget(t *testing.T) {
	s, in, add := suppressionFixture(t)
	for i := 0; i < 23; i++ {
		add(fmt.Sprint(i))
	}
	r := s.findingRecorder()
	seen := map[int64]bool{}
	calls, chars := 0, 0
	r.judge = func(_ context.Context, _ db.RecordFindingInput, rules []db.FindingSuppressionRule) (suppressionVerdict, error) {
		calls++
		chars += suppressionInputSize(in, rules)
		for _, rule := range rules {
			if seen[rule.ID] {
				t.Fatal("rule repeated")
			}
			seen[rule.ID] = true
		}
		return suppressionVerdict{Decision: "allow", Reason: "different vulnerability"}, nil
	}
	started := time.Now()
	if _, err := r.Record(t.Context(), in, nil); err != nil || len(seen) != 23 {
		t.Fatalf("missed page %d %v", len(seen), err)
	}
	t.Logf("23 rules: calls=%d input_unicode_chars=%d elapsed_ms=%d (mock model)", calls, chars, time.Since(started).Milliseconds())
	in.Summary = strings.Repeat("界", 25000)
	out, err := r.Record(t.Context(), in, nil)
	if err != nil || out.SuppressionCheck.Status != "unchecked" {
		t.Fatal("oversized inputs silently truncated", out, err)
	}
}

func TestFindingSuppressionFailureBoundaries(t *testing.T) {
	s, in, add := suppressionFixture(t)
	add("")
	r := s.findingRecorder()
	r.judge = func(context.Context, db.RecordFindingInput, []db.FindingSuppressionRule) (suppressionVerdict, error) {
		return suppressionVerdict{}, &suppressionInfrastructureError{errors.New("database unavailable")}
	}
	if _, err := r.Record(t.Context(), in, nil); err == nil {
		t.Fatal("database error failed open")
	}
	r.judge = func(context.Context, db.RecordFindingInput, []db.FindingSuppressionRule) (suppressionVerdict, error) {
		return suppressionVerdict{}, errors.New("model offline")
	}
	checks := 0
	in.Revalidate = func(context.Context) error { checks++; return errors.New("asset revoked during review") }
	if _, err := r.Record(t.Context(), in, nil); err == nil || checks != 1 {
		t.Fatal("authorization error failed open", err)
	}
	var count int
	if err := s.m.pg.QueryRow(`SELECT count(*) FROM findings WHERE task_id=$1`, in.TaskID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failure residue", count, err)
	}
}

func TestFindingSuppressionSemaphoreAndQueueBudget(t *testing.T) {
	s, in, add := suppressionFixture(t)
	add("")
	r := s.findingRecorder()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var calls atomic.Int32
	r.judge = func(ctx context.Context, _ db.RecordFindingInput, _ []db.FindingSuppressionRule) (suppressionVerdict, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return suppressionVerdict{}, ctx.Err()
		}
		return suppressionVerdict{Decision: "allow", Reason: "different targets"}, nil
	}
	version, err := s.m.pg.FindingSuppressionVersion(t.Context(), in.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 4)
	for range 4 {
		go func() { _, err := r.check(t.Context(), t.Context(), in, version); done <- err }()
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("review did not start")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	check, err := r.check(t.Context(), ctx, in, version)
	close(release)
	for range 4 {
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	}
	if err != nil || check.Status != "unchecked" || calls.Load() != 4 {
		t.Fatal("queue budget or concurrency limit", check, err, calls.Load())
	}
}

func TestFindingSuppressionDirectDeferredTools(t *testing.T) {
	s, in, add := suppressionFixture(t)
	add("禁止此漏洞")
	r := s.findingRecorder()
	r.judge = func(_ context.Context, _ db.RecordFindingInput, rules []db.FindingSuppressionRule) (suppressionVerdict, error) {
		return suppressionVerdict{"deny", []int64{rules[0].ID}, "same finding"}, nil
	}
	for _, role := range []string{"planner", "worker", "mainagent"} {
		ts := agent.NewToolSet(s.m.pg.Exploration(in.ExplorationID), role)
		ts.SetTaskID(in.TaskID)
		ts.SetFindingRecorder(r)
		notified := false
		ts.SetNotifyFinding(func(int64, string, ...int64) { notified = true })
		var tools []actool.CoreTool
		switch role {
		case "planner":
			tools = ts.PlannerTools()
		case "worker":
			tools = ts.WorkerTools()
		default:
			tools = ts.MainAgentTools()
		}
		report := workflowTool(t, tools, "report_finding")
		ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: in.TaskID, AgentKey: role})
		params := map[string]any{"vulnclass": "renamed", "summary": "same endpoint", "severity": "high"}
		workflowCall(t, ctx, report, params, true)
		extra := actool.NewExecuteExtraTool(actool.NewRegistry(report), nil)
		workflowCall(t, ctx, extra, map[string]any{"tool_name": "report_finding", "params": params}, true)
		if notified {
			t.Fatal("denial triggered Reporter", role)
		}
	}
}

type suppressionModel struct {
	response string
	request  llm.CompletionRequest
	info     agent.RunInfo
}

func (p *suppressionModel) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.request = req
	p.info = agent.RunInfoFrom(ctx)
	return func(yield func(llm.StreamEvent, error) bool) {
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: p.response}, nil)
	}
}
func (p *suppressionModel) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return accumulateStreamForTest(ctx, p.Stream, req)
}
func TestFindingSuppressionActualModelEnvelope(t *testing.T) {
	s, in, add := suppressionFixture(t)
	rule := add("仅该接口证据不足，其他资产不受影响")
	p := &suppressionModel{response: fmt.Sprintf(`{"decision":"deny","rule_ids":[%d],"reason":"same target even after rename"}`, rule)}
	s.llmOn, s.llmProv = true, p
	_, err := s.findingRecorder().Record(t.Context(), in, nil)
	if err == nil {
		t.Fatal("actual model verdict bypassed")
	}
	if len(p.request.Tools) != 0 || len(p.request.Messages) != 1 || p.info.AgentKey != "finding_suppression" || p.info.TaskID != in.TaskID {
		t.Fatal("review context/role contamination")
	}
	body, _ := json.Marshal(p.request)
	for _, forbidden := range []string{"private proof", "new evidence"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatal("evidence leaked to classifier")
		}
	}
	for _, response := range []string{`{"decision":"allow","reason":"different target"} trailing`, `{"decision":"allow","reason":"different target","extra":true}`} {
		p.response = response
		in.Name += "distinct cache key"
		out, err := s.findingRecorder().Record(t.Context(), in, nil)
		if err != nil || out.SuppressionCheck.Status != "unchecked" {
			t.Fatal("malformed verdict not marked", out, err)
		}
	}
}

func TestFindingSuppressionAssetRevokedDuringReview(t *testing.T) {
	s, in, add := suppressionFixture(t)
	add("scope rule")
	assets := s.m.pg.Assets()
	id, err := assets.UpsertRootDomain(db.UpsertRootDomainReq{Domain: "suppression.test", TaskID: in.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if err := assets.ApproveTaskAssets(in.TaskID, []int64{id}, "user", ""); err != nil {
		t.Fatal(err)
	}
	r := s.findingRecorder()
	r.judge = func(context.Context, db.RecordFindingInput, []db.FindingSuppressionRule) (suppressionVerdict, error) {
		if err := assets.RevokeTaskAssets(in.TaskID, []int64{id}, "user", "during review"); err != nil {
			t.Fatal(err)
		}
		return suppressionVerdict{Decision: "unknown", Reason: "uncertain"}, nil
	}
	ts := agent.NewToolSet(s.m.pg.Exploration(in.ExplorationID), "mainagent")
	ts.SetTaskID(in.TaskID)
	ts.SetAssetStore(assets, s.m.pg.Companies())
	ts.SetFindingRecorder(r)
	ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: in.TaskID, AgentKey: "mainagent"})
	report := workflowTool(t, ts.MainAgentTools(), "report_finding")
	message := workflowCall(t, ctx, report, map[string]any{"vulnclass": "IDOR", "severity": "high", "summary": "fixture", "asset_ids": []int64{id}}, true)
	if !strings.Contains(message, "授权") {
		t.Fatal("wrong denial", message)
	}
}

func TestFindingSuppressionFeedbackBindingMigration(t *testing.T) {
	s, _, _ := suppressionFixture(t)
	const key = "list_finding_deletion_feedback"
	const flag = "finding_suppression_feedback_roles_v1"
	old, _ := s.m.pg.GetTool(key)
	value, existed, err := s.m.pg.GetSetting(flag)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			s.m.pg.SetSetting(flag, value)
		} else {
			s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, flag)
		}
		if old != nil {
			raw, _ := json.Marshal(old.Agents)
			s.m.pg.Exec(`UPDATE tools SET agents=$2, enabled=$3 WHERE key=$1`, key, string(raw), old.Enabled)
		}
	})
	if err := s.m.pg.SeedTool(key, "feedback", nil, json.RawMessage(`["planner"]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.pg.Exec(`UPDATE tools SET agents='["planner"]',enabled=false WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	s.m.pg.Exec(`DELETE FROM settings WHERE key=$1`, flag)
	s.seedFindingSuppressionBindings()
	tool, err := s.m.pg.GetTool(key)
	if err != nil || tool.Enabled || len(tool.Agents) != 3 {
		t.Fatal("migration overwrote settings or missed role", tool, err)
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", key); err != nil {
		t.Fatal(err)
	}
	s.seedFindingSuppressionBindings()
	tool, err = s.m.pg.GetTool(key)
	if err != nil || len(tool.Agents) != 2 {
		t.Fatal("migration reapplied over user choice", tool, err)
	}
}
