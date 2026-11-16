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

func TestRequestFilteringKeepsCompleteWriteHistory(t *testing.T) {
	req := llm.CompletionRequest{System: []string{"保留任务目标和约束"}, Messages: []llm.Message{llm.UserText("核对现有资料")}}
	input, err := json.Marshal(map[string]string{"summary": strings.Repeat("观察记录", 5000)})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		id := fmt.Sprint(i)
		req.Messages = append(req.Messages,
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "record_fact", Input: input}}},
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, fmt.Sprintf(`{"node_id":%d}`, i+1), false)}},
		)
	}
	req.Messages = append(req.Messages, llm.UserText("根据以上记录给出结论"))
	for _, role := range []string{"planner", "worker"} {
		t.Run(role, func(t *testing.T) {
			ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: role, TaskID: 1})
			got, err := (assetContextProvider{taskID: 1}).filter(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Messages, req.Messages) || len(got.System) == 0 || got.System[0] != req.System[0] {
				t.Fatal("write history or required system text changed")
			}
		})
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
