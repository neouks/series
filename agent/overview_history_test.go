package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestOverviewFieldReuseKeepsScopeAndDiagnostics(t *testing.T) {
	shared := strings.Repeat("approved fixture asset; ", 100)
	makeOverview := func(id, task, status, field string) []llm.Message {
		body, _ := json.Marshal(map[string]any{"source_task_id": task, "task": map[string]any{"goal": shared}, "goals": []any{shared}, "status": status, "recent_facts": []any{field}})
		return []llm.Message{documentationCall(id, "graph_overview", `{"task_id":`+task+`}`), {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, string(body), false)}}}
	}
	req := llm.CompletionRequest{}
	req.Messages = append(req.Messages, makeOverview("old", "1", "running", "old fact")...)
	req.Messages = append(req.Messages, makeOverview("other", "2", "running", "other fact")...)
	partial := `{"source_task_id":1,"partial":true,"unavailable":{"facts":"query failed"},"task":{"goal":"diagnostic"}}`
	req.Messages = append(req.Messages, documentationCall("partial", "graph_overview", `{"task_id":1}`), llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("partial", partial, false)}})
	req.Messages = append(req.Messages, makeOverview("new", "1", "passed", "new fact")...)
	before, _ := json.Marshal(req)
	out := reuseOverviewFields(req)
	if got, _ := json.Marshal(req); string(got) != string(before) {
		t.Fatal("raw request changed")
	}
	if !reflect.DeepEqual(out, reuseOverviewFields(out)) {
		t.Fatal("projection is not stable")
	}
	old := out.Messages[1].Content[0].Content[0].Text
	if !strings.Contains(old, `"tool_use_id":"new"`) || !strings.Contains(old, `"status":"running"`) || !strings.Contains(old, "old fact") {
		t.Fatal("old snapshot lost changes or reuse pointer", old)
	}
	if !reflect.DeepEqual(out.Messages[3], req.Messages[3]) || !reflect.DeepEqual(out.Messages[5], req.Messages[5]) || !reflect.DeepEqual(out.Messages[7], req.Messages[7]) {
		t.Fatal("other task, diagnostics or latest snapshot changed")
	}
	bounded := boundRetrievalHistory(req)
	if !reflect.DeepEqual(bounded.Messages[5], req.Messages[5]) {
		t.Fatal("partial overview diagnostics were compacted")
	}
}
