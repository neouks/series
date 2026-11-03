package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func TestCompletedBatchProjectionPreservesProtocolAndAudit(t *testing.T) {
	req := llm.CompletionRequest{System: []string{"goal and required constraints"}}
	for i := 0; i < 20; i++ {
		id := fmt.Sprint(i)
		input, _ := json.Marshal(map[string]any{"summary": strings.Repeat("证据", 6000)})
		req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "record_fact", Input: input}}},
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, fmt.Sprintf(`{"node_id":%d}`, i+1), false)}})
	}
	before, _ := json.Marshal(req)
	out := projectCompletedBatches(req)
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("audit changed")
	}
	if !reflect.DeepEqual(out.System, req.System) {
		t.Fatal("constraints changed")
	}
	calls, results := 0, 0
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				calls++
			}
			if b.Type == llm.BlockToolResult {
				results++
			}
		}
	}
	if calls != 2 || results != 2 {
		t.Fatalf("pairing %d/%d", calls, results)
	}
	if out.Messages[len(out.Messages)-1].Content[0].ToolUseID != "19" {
		t.Fatal("latest batch changed")
	}
	if measureRequest(out).Characters >= measureRequest(req).Characters/3 {
		t.Fatal("long arguments retained")
	}
	t.Logf("20_write_batches before_chars=%d after_chars=%d", measureRequest(req).Characters, measureRequest(out).Characters)
	// An ordinary user message breaks a batch; no cross-boundary pairing.
	broken := llm.CompletionRequest{Messages: append([]llm.Message{}, req.Messages...)}
	broken.Messages = append(broken.Messages[:1], append([]llm.Message{llm.UserText("new user boundary")}, broken.Messages[1:]...)...)
	got := projectCompletedBatches(broken)
	if got.Messages[0].Content[0].Type != llm.BlockToolUse {
		t.Fatal("incomplete batch modified")
	}
}

func TestWorkerTodoProgressIsBoundedDurableAndScoped(t *testing.T) {
	dir := t.TempDir()
	state, err := loadWorkerTodos(dir, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	tool := workerTodoTool{CoreTool: state.store.Tool(), state: state}
	items := []actool.Todo{{Content: "第一步\n必须保留这一行", Status: actool.TodoInProgress}}
	for range 100 {
		items = append(items, actool.Todo{Content: strings.Repeat("长执行待办", 500), Status: actool.TodoPending})
	}
	input, _ := json.Marshal(map[string]any{"todos": items})
	ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "worker", TaskID: 1, IntentID: 2})
	if r, e := tool.Call(ctx, input, nil); e != nil || r.IsError {
		t.Fatal(r, e)
	}
	restored, err := loadWorkerTodos(dir, 1, 2)
	if err != nil || !reflect.DeepEqual(restored.store.List(), items) {
		t.Fatal("progress not restored", err)
	}
	request := refreshWorkerTodos(context.WithValue(ctx, workerTodoKey{}, restored), llm.CompletionRequest{})
	if len(request.System) != 1 || utf8.RuneCountInString(request.System[0]) > 3600 {
		t.Fatal("unbounded progress preview")
	}
	if !strings.Contains(request.System[0], "必须保留这一行") {
		t.Fatal("multiline progress lost")
	}
	if r, _ := tool.Call(WithRunInfo(t.Context(), RunInfo{AgentKey: "worker", TaskID: 1, IntentID: 3}), input, nil); !r.IsError {
		t.Fatal("wrong intent overwrote progress")
	}
	if mode, e := os.Stat(state.path); e != nil || mode.Mode().Perm() != 0600 {
		t.Fatal("progress file permissions", e)
	}
}

func TestContextSoftBudgetWarnsContinuesAndPreservesLatestFailure(t *testing.T) {
	var events []db.Activity
	ctx := withContextObserver(WithRunInfo(t.Context(), RunInfo{AgentKey: "worker", TaskID: 999901, IntentID: 7}), func(a db.Activity) { events = append(events, a) })
	required := strings.Repeat("必要操作限制", 20000)
	req := llm.CompletionRequest{System: []string{required}, Messages: []llm.Message{llm.UserText("latest user instruction")}}
	out := projectRequestBudget(ctx, req)
	projectRequestBudget(ctx, req)
	if out.System[0] != required || len(events) != 1 {
		t.Fatal("constraints lost or warning repeated", len(events))
	}
	if measureRequest(out).Characters <= 48000 {
		t.Fatal("fixture did not exceed budget")
	}
}

func TestPlannerTodoAndWorkerLaunchBudgets(t *testing.T) {
	items := []actool.Todo{}
	for range 100 {
		items = append(items, actool.Todo{Content: strings.Repeat("长待办", 1000), Status: actool.TodoCompleted})
	}
	for range 40 {
		items = append(items, actool.Todo{Content: strings.Repeat("当前待办", 500), Status: actool.TodoPending})
	}
	text := renderPlannerTodos(items)
	if utf8.RuneCountInString(text) > 3000 || strings.Contains(text, "长待办长待办") {
		t.Fatal("unbounded todo preview")
	}
	payload, _ := json.Marshal(map[string]any{"summary": strings.Repeat("长意图", 10000), "secret_other_field": "not-for-system"})
	mandate := renderIntentTask(&db.Node{ID: 7, Payload: payload})
	if utf8.RuneCountInString(mandate) > 2400 || strings.Contains(mandate, "not-for-system") || !strings.Contains(mandate, "node:7") {
		t.Fatal("unbounded mandate", utf8.RuneCountInString(mandate))
	}
}

func TestPlannerWorkingStateReadRoleAndPagination(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("planner state", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	s := d.Exploration(task.ExplorationID)
	ts := NewToolSet(s, "planner")
	ts.SetTaskID(task.ID)
	items := []actool.Todo{{Content: strings.Repeat("中文🙂", 1000), Status: actool.TodoPending}, {Content: "second", Status: actool.TodoInProgress}}
	store := actool.NewTodoStore()
	tool := persistentPlannerTodo{CoreTool: store.Tool(), store: s}
	raw, _ := json.Marshal(map[string]any{"todos": items})
	if r, err := tool.Call(WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: task.ID, ExplorationID: s.ID()}), raw, nil); err != nil || r.IsError {
		t.Fatal(r, err)
	}
	planner := NewPlanner(nil, "", "", nil, 0, 0)
	restored, err := planner.restoreTodos(t.Context(), s)
	if err != nil || !reflect.DeepEqual(restored.List(), items) {
		t.Fatal("restore lost todos", err)
	}
	ref := fmt.Sprintf("planner_state:%d", task.ID)
	for _, role := range []string{"worker", "mainagent", "planner"} {
		ctx := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: role})
		out, err := ts.readContextItem(ctx, "", contextRead{Ref: ref}, 400)
		if role != "planner" {
			if err == nil {
				t.Fatalf("%s read planner-only state: %v", role, out)
			}
			continue
		}
		if err != nil || out["next"] == nil {
			t.Fatal("no bounded continuation", out, err)
		}
	}
	ctx := WithRunInfo(t.Context(), RunInfo{TaskID: task.ID, AgentKey: "planner"})
	if _, err = ts.readContextItem(ctx, "9999", contextRead{Ref: ref}, 500); err == nil {
		t.Fatal("cross-task planner state allowed")
	}
	item := contextRead{Ref: ref}
	var recovered strings.Builder
	for range 100 {
		out, e := ts.readContextItem(ctx, "", item, 400)
		if e != nil {
			t.Fatal(e)
		}
		for _, row := range out["items"].([]map[string]any) {
			recovered.WriteString(row["content"].(string))
		}
		next, ok := out["next"].(map[string]any)
		if !ok {
			break
		}
		item = next["items"].([]contextRead)[0]
	}
	if recovered.String() != items[0].Content+items[1].Content {
		t.Fatal("todo Unicode continuation lost content")
	}
}

func TestIncrementalSummaryRejectsInventedReferencesAndEvidenceUpgrade(t *testing.T) {
	sources := []db.GraphCandidate{{ID: 1, Confidence: "inferred"}}
	for _, raw := range []string{`{"items":[{"text":"invented","refs":[2],"confidence":"inferred"}]}`, `{"items":[{"text":"upgraded","refs":[1],"confidence":"observed"}]}`, `{"items":[]}`, `not JSON`} {
		if _, err := validateGraphSummary(raw, sources); err == nil {
			t.Fatal("accepted invalid summary", raw)
		}
	}
	if _, err := validateGraphSummary(`{"items":[{"text":"uncertain outcome","refs":[1],"confidence":"inferred"}]}`, sources); err != nil {
		t.Fatal(err)
	}
}

func TestRequestMeasurementAndLongRetrievalProjection(t *testing.T) {
	for _, pages := range []int{2, 10, 100, 300} {
		req := retrievalFixture(pages)
		req.System = append(req.System, "必要约束 <>& 中文🙂")
		raw, _ := json.Marshal(req)
		got := measureRequest(req)
		if got.Bytes != len(raw) || got.Characters != utf8.RuneCount(raw) {
			t.Fatal("measurement mismatch", got, len(raw))
		}
		bounded := boundRetrievalHistory(req)
		projected := projectRequestBudget(WithRunInfo(t.Context(), RunInfo{AgentKey: "worker", TaskID: 999902}), bounded)
		if projected.System[0] != req.System[0] {
			t.Fatal("system lost")
		}
		if pages >= 100 && measureRequest(projected).Characters > 48000 {
			t.Fatalf("unbounded index after %d pages: %d", pages, measureRequest(projected).Characters)
		}
		t.Logf("pages=%d original_chars=%d read_window_chars=%d projected_chars=%d projected_bytes=%d", pages, got.Characters, measureRequest(bounded).Characters, measureRequest(projected).Characters, measureRequest(projected).Bytes)
	}
}

func TestPlannerStateHistoryCannotCrossRole(t *testing.T) {
	req := llm.CompletionRequest{Messages: []llm.Message{{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "state", Name: "context_get", Input: json.RawMessage(`{"items":[{"ref":"planner_state:1"}]}`)}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("state", `{"items":[{"ref":"planner_state:1","items":[{"content":"private planner todo"}]}]}`, false)}}}}
	out := refreshPlannerStateHistory(WithRunInfo(t.Context(), RunInfo{AgentKey: "worker", TaskID: 1}), req)
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "private planner todo") {
		t.Fatal("planner state crossed role")
	}
	if !strings.Contains(req.Messages[1].Content[0].Content[0].Text, "private planner todo") {
		t.Fatal("audit mutated")
	}
}

func TestReadQuerySemanticNormalizationPreservesPaging(t *testing.T) {
	if normalizedReadQuery("context_search", json.RawMessage(`{"task_id":132}`)) != normalizedReadQuery("context_search", json.RawMessage(`{"task_id":"132"}`)) {
		t.Fatal("task ID integer/string alias not normalized")
	}
	if normalizedReadQuery("context_search", json.RawMessage(`{"task_id":132}`)) == normalizedReadQuery("context_search", json.RawMessage(`{"task_id":133}`)) {
		t.Fatal("different task scopes deduplicated")
	}
	a := normalizedReadQuery("context_search", json.RawMessage(`{"types":["finding","fact"]}`))
	b := normalizedReadQuery("context_search", json.RawMessage(`{"types":["fact","finding","fact"]}`))
	if a != b {
		t.Fatal("set-valued types not normalized")
	}
	a = normalizedReadQuery("context_get", json.RawMessage(`{"items":[{"ref":"node:1","view":"detail","offset":0}]}`))
	b = normalizedReadQuery("context_get", json.RawMessage(`{"items":[{"ref":"node:1"}]}`))
	if a != b {
		t.Fatal("default read view not normalized")
	}
	if a == normalizedReadQuery("context_get", json.RawMessage(`{"items":[{"ref":"node:1","offset":1}]}`)) {
		t.Fatal("different text pages deduplicated")
	}
}
