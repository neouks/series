package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func TestContextReadsAuthorizationAndCursor(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, e := d.CreateTaskWithOptions("context access", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	as := d.Assets()
	es := d.Exploration(task.ExplorationID)
	asset, e := as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: fmt.Sprintf("context-%d.test", task.ID), TaskID: task.ID, AgentDiscovered: true})
	if e != nil {
		t.Fatal(e)
	}
	var ids []int64
	for i := 0; i < 3; i++ {
		id, e := es.AddNode(db.KindFact, map[string]any{"summary": "brief", "detail": strings.Repeat("中文🙂", 1000) + "NeEdLe_%'"}, 0, "confirmed", "test", []int64{asset})
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	ts := NewToolSet(es, "test")
	ts.SetTaskID(task.ID)
	ts.SetAssetStore(as, d.Companies())
	call := func(tool actool.CoreTool, ctxRole string, in any, tc *actool.ToolContext) map[string]any {
		t.Helper()
		ctx := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, ExplorationID: task.ExplorationID, AgentKey: ctxRole})
		if ctxRole == "worker" {
			ctx = WithRunInfo(ctx, RunInfo{IntentID: 99})
		}
		b, _ := json.Marshal(in)
		r, e := tool.Call(ctx, b, tc)
		if e != nil || r.IsError {
			t.Fatalf("%s: %s %v", tool.Name(), r.Flatten(), e)
		}
		var out map[string]any
		if e = json.Unmarshal([]byte(r.Flatten()), &out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	q := map[string]any{"types": []string{"fact"}, "q": "needle_%'", "limit": 2}
	if rows := call(ts.contextSearch(), "planner", q, nil)["items"].([]any); len(rows) != 0 {
		t.Fatal("planner read pending", rows)
	}
	var mainSearch actool.CoreTool
	for _, tool := range ts.MainAgentTools() {
		if tool.Name() == "context_search" {
			mainSearch = tool
		}
	}
	first := call(mainSearch, "mainagent", q, nil)
	rows := first["items"].([]any)
	if len(rows) != 2 || first["has_more"] != true {
		t.Fatal(first)
	}
	if !strings.Contains(rows[0].(map[string]any)["snippet"].(string), "NeEdLe_%'") {
		t.Fatal("body snippet missing")
	}
	q["cursor"] = first["next_cursor"]
	if _, e = es.AddNode(db.KindFact, map[string]any{"summary": "needle_%' new"}, 0, "confirmed", "test", []int64{asset}); e != nil {
		t.Fatal(e)
	}
	if second := call(mainSearch, "mainagent", q, nil); len(second["items"].([]any)) != 1 {
		t.Fatal("unstable page", second)
	}
	ts.workerExecution = true
	if rows := call(ts.contextSearch(), "worker", map[string]any{"types": []string{"fact"}, "q": "needle"}, nil)["items"].([]any); len(rows) != 4 {
		t.Fatal("worker pending blocked", rows)
	}
	ts.workerExecution = false
	// Mixed references fail independently; low output budgets preserve valid JSON and continuations.
	var mainGet actool.CoreTool
	for _, tool := range ts.MainAgentTools() {
		if tool.Name() == "context_get" {
			mainGet = tool
		}
	}
	get := map[string]any{"items": []any{map[string]any{"ref": fmt.Sprintf("node:%d", ids[0]), "field": "/detail"}, map[string]any{"ref": "finding:123"}}, "max_chars": 24000}
	result := call(mainGet, "mainagent", get, &actool.ToolContext{MaxOutputChars: 4000})
	items := result["items"].([]any)
	if items[0].(map[string]any)["payload"].(map[string]any)["next_offset"] == nil || items[1].(map[string]any)["error"] == nil {
		t.Fatal(result)
	}
	original, _ := json.Marshal(result)
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "read", Name: "context_get", Input: json.RawMessage(`{}`)}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "read", Content: []llm.ContentBlock{{Type: llm.BlockText, Text: string(original)}}}}}}}
	if e = as.RevokeTaskAssets(task.ID, []int64{asset}, "user", ""); e != nil {
		t.Fatal(e)
	}
	if last := call(mainSearch, "mainagent", q, nil); len(last["items"].([]any)) != 0 {
		t.Fatal("revocation bypass")
	}
	filtered, e := FilterTaskAssetRequest(WithRunInfo(t.Context(), RunInfo{AgentKey: "mainagent", TaskID: task.ID}), as, task.ID, req)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(filtered)
	if strings.Contains(string(b), "中文") {
		t.Fatal("revoked historical body remained")
	}
	b, _ = json.Marshal(req)
	if !strings.Contains(string(b), "中文") {
		t.Fatal("original audit changed")
	}
}

func TestContextCapabilityBoundaries(t *testing.T) {
	ts := NewToolSet(nil, "test")
	ctx := WithContextAccess(t.Context(), ContextAccess{Local: []string{"finding"}})
	for _, in := range []string{`{"types":["fact"]}`, `{"task_id":"99","types":["finding"]}`, `{"types":["activity"]}`} {
		r, e := ts.contextSearch().Call(ctx, json.RawMessage(in), nil)
		if e != nil || !r.IsError {
			t.Fatalf("unexpected grant %s: %v %v", in, r, e)
		}
	}
	for _, ref := range []string{"123", "finding:1", "node:0", "node:-1", "activity:1:2"} {
		if _, _, e := parseContextRef(ref); e == nil {
			t.Fatal(ref)
		}
	}
}

func TestContextReadHistoryDedup(t *testing.T) {
	for _, name := range []string{"context_search", "context_get", "asset_search", "asset_get", "traffic_get", "traffic_search"} {
		req := llm.CompletionRequest{Messages: []llm.Message{}}
		for _, id := range []string{"old", "new"} {
			req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(`{"q":"x"}`)}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: id, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "body-" + id}}}}})
		}
		out := deduplicateToolHistory(req)
		b, _ := json.Marshal(out)
		if strings.Contains(string(b), "body-old") || !strings.Contains(string(b), "body-new") {
			t.Fatal(name, string(b))
		}
		if req.Messages[1].Content[0].Content[0].Text != "body-old" {
			t.Fatal("changed original")
		}
	}
}

func TestOverviewHotProjectionMatchesCompactionGraph(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, e := d.CreateExploration("hot projection", "goal")
	if e != nil {
		t.Fatal(e)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	ids := []int64{}
	for _, kind := range []string{db.KindFact, db.KindIntent, db.KindFact, db.KindFact, db.KindDigest} {
		state := "confirmed"
		if kind == db.KindIntent {
			state = "open"
		}
		if kind == db.KindDigest {
			state = "active"
		}
		id, e := s.AddNode(kind, map[string]any{"summary": "node", "body": strings.Repeat("large", 10000)}, 0, state, "test", nil)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	if e = s.SetIntentState(ids[1], "running"); e != nil {
		t.Fatal(e)
	}
	for _, edge := range []db.Edge{{From: ids[0], To: ids[1], Rel: db.RelDerivedFrom}, {From: ids[1], To: ids[2], Rel: db.RelYields}, {From: ids[2], To: ids[0], Rel: db.RelDerivedFrom}, {From: ids[4], To: ids[3], Rel: db.RelCovers}} {
		if e = s.Link(edge.From, edge.Rel, edge.To); e != nil {
			t.Fatal(e)
		}
	}
	cg, _, e := loadColdGraph(s)
	if e != nil {
		t.Fatal(e)
	}
	got, e := s.ToolHotNodeIDs(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(got, cg.hotSet()) {
		t.Fatal(got, cg.hotSet())
	}
}

func TestContextOutputAndDigestViews(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, e := d.CreateTask("context output", "goal", nil, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	exp := task.ExplorationID

	s := d.Exploration(exp)
	ts := NewToolSet(s, "test")
	intent, e := s.AddIntent(map[string]any{"summary": "work"}, 1, nil, "test")
	if e != nil {
		t.Fatal(e)
	}
	read := func(id int64, view string) map[string]any {
		t.Helper()
		r, e := ts.contextGet().Call(t.Context(), json.RawMessage(fmt.Sprintf(`{"items":[{"ref":"node:%d","view":%q}]}`, id, view)), nil)
		if e != nil || r.IsError {
			t.Fatal(r.Flatten(), e)
		}
		var out map[string]any
		if e = json.Unmarshal([]byte(r.Flatten()), &out); e != nil {
			t.Fatal(e)
		}
		return out["items"].([]any)[0].(map[string]any)
	}
	if e = s.SetIntentState(intent, "running"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AppendActivity(db.Activity{NodeID: &intent, Kind: "text", Detail: "intermediate"}); e != nil {
		t.Fatal(e)
	}
	if row := read(intent, "output"); row["is_final"] != false || row["text"] != "intermediate" || row["final_text"] != nil {
		t.Fatal(row)
	}
	if _, e = s.AppendActivity(db.Activity{NodeID: &intent, Kind: "result", Detail: "complete"}); e != nil {
		t.Fatal(e)
	}
	if e = s.SetIntentState(intent, "done"); e != nil {
		t.Fatal(e)
	}
	if row := read(intent, "output"); row["is_final"] != true || row["text"] != "complete" {
		t.Fatal(row)
	}
	if e = s.SetIntentState(intent, "open"); e != nil {
		t.Fatal(e)
	}
	if e = s.SetIntentState(intent, "running"); e != nil {
		t.Fatal(e)
	}
	if row := read(intent, "output"); row["has_output"] != false {
		t.Fatal("previous execution leaked", row)
	}
	fact, e := s.AddNode(db.KindFact, map[string]any{"summary": "member", "detail": strings.Repeat("body", 10000)}, 0, "confirmed", "test", nil)
	if e != nil {
		t.Fatal(e)
	}
	digest, e := s.AddNode(db.KindDigest, map[string]any{"body": "digest"}, 0, "active", "test", nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Link(digest, db.RelCovers, fact); e != nil {
		t.Fatal(e)
	}
	row := read(digest, "members")
	members, ok := row["members"].([]any)
	if !ok || len(members) != 1 || members[0].(map[string]any)["ref"] != fmt.Sprintf("node:%d", fact) {
		t.Fatal(row)
	}
	b, _ := json.Marshal(row)
	if strings.Contains(string(b), "bodybody") {
		t.Fatal("loaded member body")
	}
}

func TestContextAssetProjectionAndTrafficRefresh(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, e := d.CreateTaskWithOptions("projection", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets"})
	if e != nil {
		t.Fatal(e)
	}
	defer d.DeleteTask(task.ID)
	host := fmt.Sprintf("projection-%d.test", task.ID)
	as := d.Assets()
	id, e := as.UpsertRootDomain(db.UpsertRootDomainReq{Domain: host, TaskID: task.ID, AgentDiscovered: true})
	if e != nil {
		t.Fatal(e)
	}
	secret := strings.Repeat("中文🙂", 10000)
	extra, _ := json.Marshal(map[string]any{"large": secret})
	if _, e = d.Exec(`UPDATE assets SET extra=$2::jsonb,auth=ARRAY[jsonb_build_object('password','test-secret')] WHERE id=$1`, id, string(extra)); e != nil {
		t.Fatal(e)
	}
	ts := NewToolSet(d.Exploration(task.ExplorationID), "test")
	ts.SetTaskID(task.ID)
	ts.SetAssetStore(as, d.Companies())
	ctx := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "mainagent"})
	var get actool.CoreTool
	for _, tool := range ts.MainAgentTools() {
		if tool.Name() == "asset_get" {
			get = tool
		}
	}
	call := func(in any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(in)
		r, e := get.Call(ctx, b, &actool.ToolContext{MaxOutputChars: 4000})
		if e != nil || r.IsError {
			t.Fatal(r.Flatten(), e)
		}
		if len(r.Flatten()) > 4000 {
			t.Fatal("overflow")
		}
		var out map[string]any
		if e = json.Unmarshal([]byte(r.Flatten()), &out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	initial := call(map[string]any{"ids": []int64{id, 9223372036854775806}})
	b, _ := json.Marshal(initial)
	if strings.Contains(string(b), "test-secret") || strings.Contains(string(b), "large") {
		t.Fatal("implicit sensitive fields")
	}
	rows := initial["assets"].([]any)
	if len(rows) != 2 || rows[1].(map[string]any)["error"] == nil {
		t.Fatal(initial)
	}
	input := map[string]any{"ids": []int64{id}, "fields": []string{"extra"}, "field": "/extra/large", "max_chars": 24000}
	var joined strings.Builder
	for i := 0; ; i++ {
		if i > 1000 {
			t.Fatal("stalled cursor")
		}
		out := call(input)
		row := out["assets"].([]any)[0].(map[string]any)
		joined.WriteString(row["details"].(map[string]any)["value"].(string))
		next, ok := row["next"].(map[string]any)
		if !ok {
			break
		}
		input = next
	}
	if joined.String() != secret {
		t.Fatal("asset body not lossless")
	}
	if e = as.AddAutoScope(task.ID, "root_domain", host, "", ""); e != nil {
		t.Fatal(e)
	}
	for _, tool := range ts.MainAgentTools() {
		if tool.Name() == "asset_search" {
			in, _ := json.Marshal(map[string]any{"untested": true, "dsl": "domain==" + host})
			r, e := tool.Call(ctx, in, nil)
			if e != nil || r.IsError {
				t.Fatal(r.Flatten(), e)
			}
			var out map[string]any
			json.Unmarshal([]byte(r.Flatten()), &out)
			if len(out["assets"].([]any)) != 1 {
				t.Fatal("untested pending visibility", out)
			}
		}
	}

	body, _ := json.Marshal(map[string]any{"target_host": host, "parts": []any{map[string]any{"text": "private-traffic"}}})
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "traffic", Name: "traffic_get", Input: json.RawMessage(`{"id":"one"}`)}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "traffic", Content: []llm.ContentBlock{{Type: llm.BlockText, Text: string(body)}}}}}}}
	allowed, e := FilterTaskAssetRequest(ctx, as, task.ID, req)
	if e != nil {
		t.Fatal(e)
	}
	b, _ = json.Marshal(allowed)
	if !strings.Contains(string(b), "private-traffic") {
		t.Fatal("pending main hidden")
	}
	if e = as.RevokeTaskAssets(task.ID, []int64{id}, "user", ""); e != nil {
		t.Fatal(e)
	}
	filtered, e := FilterTaskAssetRequest(ctx, as, task.ID, req)
	if e != nil {
		t.Fatal(e)
	}
	b, _ = json.Marshal(filtered)
	if strings.Contains(string(b), "private-traffic") {
		t.Fatal("revoked traffic remains")
	}
	b, _ = json.Marshal(req)
	if !strings.Contains(string(b), "private-traffic") {
		t.Fatal("audit mutated")
	}
}
