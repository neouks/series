package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestFindingReportToolScopeAndEvidenceIDCompatibility(t *testing.T) {
	s, f, _ := trafficEvidenceServer(t)
	pg := s.m.pg
	stored, err := pg.GetFinding(f.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	owner := *stored.TaskID
	report := agent.ResolveBuiltinTool(s.toolUpdateFindingReport(), "", nil)
	read := agent.ResolveBuiltinTool(s.toolGetFindingTraffic(), "", nil)
	ownerCtx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: owner, AgentKey: "reporter"})
	for _, role := range []string{"planner", "worker", "mainagent"} {
		ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{AgentKey: role})
		workflowCall(t, ctx, read, map[string]any{"finding_id": f.FindingID}, true)
		workflowCall(t, ctx, report, map[string]any{"finding_node_id": f.NodeID, "report": "MISSING_CONTEXT"}, true)
	}
	for _, id := range []any{f.NodeID, fmt.Sprint(f.NodeID)} {
		workflowCall(t, ownerCtx, report, map[string]any{"finding_node_id": id, "report": "OWNER_REPORT", "evidence_version": 0}, false)
	}
	for _, sources := range [][]int64{nil, {owner}} {
		other, err := s.m.CreateTaskWithOptions("scope isolation", "fixture", db.TaskCreateOptions{SourceTaskIDs: sources})
		if err != nil {
			t.Fatal(err)
		}
		var otherID int64
		fmt.Sscan(other.ID, &otherID)
		t.Cleanup(func() { pg.DeleteTask(otherID) })
		ctx := agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: otherID, AgentKey: "mainagent"})
		workflowCall(t, ctx, report, map[string]any{"finding_node_id": f.NodeID, "report": "FOREIGN_WRITE", "evidence_version": 0}, true)
		workflowCall(t, ctx, read, map[string]any{"finding_id": fmt.Sprint(f.FindingID)}, len(sources) == 0)
	}
	current, err := pg.GetFinding(f.FindingID)
	if err != nil || current.Report != "OWNER_REPORT" {
		t.Fatal("unauthorized write changed report", current, err)
	}
	seedServerEvidenceFlow(t, s, "compatibility-body", []byte("compatibility evidence"))
	bound, err := s.evidenceStore().Bind(t.Context(), f.FindingID, []db.TrafficRef{{TrafficID: "compatibility-body"}})
	if err != nil {
		t.Fatal(err)
	}
	binding := bound.Bindings[0].ID
	for _, id := range []any{binding, fmt.Sprint(binding)} {
		result := workflowCall(t, ownerCtx, read, map[string]any{"finding_id": fmt.Sprint(f.FindingID), "binding_id": id}, false)
		if !strings.Contains(result, "compatibility evidence") {
			t.Fatal("valid string ID lost body", result)
		}
	}
	workflowCall(t, ownerCtx, report, map[string]any{"finding_node_id": f.NodeID, "report": "STALE", "evidence_version": 0}, true)
	for _, bad := range []any{0, -1, "0", "-1", "1.5", "9223372036854775808", nil} {
		workflowCall(t, ownerCtx, read, map[string]any{"finding_id": bad}, true)
		workflowCall(t, ownerCtx, report, map[string]any{"finding_node_id": bad, "report": "INVALID"}, true)
	}
	// This asset belongs to a parent fact, not findings.asset_ids. Direct-only
	// checks would let both report writes and evidence reads bypass its state.
	if _, err = pg.Exec(`UPDATE tasks SET asset_approval_template='explicit_targets' WHERE id=$1`, owner); err != nil {
		t.Fatal(err)
	}
	asset, err := pg.Assets().UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("finding-review-%d.test", owner), TaskID: owner, AgentDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	task := s.m.ResolveTask(fmt.Sprint(owner))
	parent, err := task.Store.AddNode(db.KindFact, map[string]any{"summary": "pending ancestor"}, 0, "confirmed", "test", []int64{asset})
	if err != nil {
		t.Fatal(err)
	}
	if err = task.Store.Link(parent, db.RelDerivedFrom, f.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err = pg.Exec(`UPDATE task_asset_links SET approval_state='pending' WHERE task_id=$1 AND asset_id=$2`, owner, asset); err != nil {
		t.Fatal(err)
	}
	states, err := pg.Assets().TaskAssetApprovalStates(owner, []int64{asset})
	if err != nil || states[asset] != db.ApprovalPending {
		t.Fatal("invalid pending fixture", states, err)
	}
	write := map[string]any{"finding_node_id": f.NodeID, "report": "ROLE_REPORT", "evidence_version": bound.Version}
	for _, ri := range []agent.RunInfo{{TaskID: owner, AgentKey: "planner"}, {TaskID: owner, AgentKey: "mainagent"}, {TaskID: owner, AgentKey: "worker", IntentID: 1}} {
		ctx := agent.WithRunInfo(t.Context(), ri)
		deny := ri.AgentKey == "planner"
		workflowCall(t, ctx, read, map[string]any{"finding_id": f.FindingID}, deny)
		workflowCall(t, ctx, report, write, deny)
	}
	if err = pg.Assets().RevokeTaskAssets(owner, []int64{asset}, "test", ""); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"planner", "worker", "mainagent"} {
		ri := agent.RunInfo{TaskID: owner, AgentKey: role}
		if role == "worker" {
			ri.IntentID = 1
		}
		ctx := agent.WithRunInfo(t.Context(), ri)
		workflowCall(t, ctx, read, map[string]any{"finding_id": f.FindingID}, true)
		workflowCall(t, ctx, report, write, true)
	}
}

func TestReporterDefaultPromptUpgradeUsesCurrentParameter(t *testing.T) {
	s, _, _ := trafficEvidenceServer(t)
	pg := s.m.pg
	if strings.Contains(reporterToolCallMessage, "update_finding_report(finding_id") || !strings.Contains(reporterToolCallMessage, "update_finding_report(finding_node_id,") {
		t.Fatal("report prompt does not match schema")
	}
	if _, err := pg.Exec(`DELETE FROM settings WHERE key='reporter_trigger_finding_node_id_v2'`); err != nil {
		t.Fatal(err)
	}
	expected := map[int64]string{}
	for _, body := range []string{reporterToolCallMessageV1, reporterToolCallMessageV2, "USER CUSTOM PROMPT"} {
		tr, err := pg.CreateTrigger(&db.AgentTrigger{AgentKey: "reporter", OnToolCall: true, ToolCallMessage: body, ToolNames: []string{"report_finding"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pg.DeleteTrigger(tr.ID) })
		if body == "USER CUSTOM PROMPT" {
			expected[tr.ID] = body
		} else {
			expected[tr.ID] = reporterToolCallMessage
		}
	}
	s.upgradeReporterTriggerMessage()
	s.upgradeReporterTriggerMessage()
	rows, err := pg.ListTriggersFor("reporter")
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range rows {
		if want, ok := expected[tr.ID]; ok {
			if tr.ToolCallMessage != want {
				t.Fatal(tr)
			}
			delete(expected, tr.ID)
		}
	}
	if len(expected) > 0 {
		t.Fatal("lost triggers", expected)
	}
	// Validate the documented argument name through the real catalog decorator.
	raw, _ := json.Marshal(map[string]any{"finding_node_id": 1, "report": ""})
	tool := agent.ResolveBuiltinTool(s.toolUpdateFindingReport(), "", nil)
	if _, err := agent.PrepareToolInput(tool, raw); err != nil {
		t.Fatal(err)
	}
}

func TestToolIDParsingDoesNotSaturateOrRound(t *testing.T) {
	for _, raw := range []string{`"9223372036854775808"`, `"-9223372036854775809"`, `9223372036854775808`, `1.5`, `"1.5"`, `null`, `false`} {
		if got := parseProfileID(json.RawMessage(raw)); got != 0 {
			t.Fatalf("invalid %s resolved as %d", raw, got)
		}
	}
	for _, raw := range []string{`9007199254740993`, `"9007199254740993"`} {
		if got := parseProfileID(json.RawMessage(raw)); got != 9007199254740993 {
			t.Fatalf("ID rounded: %s to %d", raw, got)
		}
	}
}
