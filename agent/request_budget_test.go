package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func appendBudgetBatch(req *llm.CompletionRequest, id, name string, result llm.ContentBlock) {
	result.ToolUseID = id
	req.Messages = append(req.Messages, llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(`{"items":[{"ref":"node:1"}]}`)}}}, llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{result}})
}

func TestCompletedErrorsKeepLatestAndMergeOnlyExactDiagnostics(t *testing.T) {
	req := llm.CompletionRequest{}
	for i := range 8 {
		failed := i < 5
		text := `{"items":[]}`
		if failed {
			text = "permission denied: original diagnostic"
		}
		appendBudgetBatch(&req, fmt.Sprint(i), "context_get", llm.ToolResultText("", text, failed))
	}
	out := projectCompletedBatches(req)
	calls := []string{}
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				calls = append(calls, b.ID)
			}
		}
	}
	if strings.Join(calls, ",") != "4,6,7" {
		t.Fatal("latest error or newest batches changed", calls)
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `\"occurrences\":4`) || !strings.Contains(string(raw), "original diagnostic") {
		t.Fatal("repeated errors not indexed", string(raw))
	}
}

func TestBatchProjectionKeepsPartialErrorsImagesAndApplicationJSON(t *testing.T) {
	for _, kind := range []string{"partial_read", "partial_write", "image", "application_json"} {
		t.Run(kind, func(t *testing.T) {
			req := llm.CompletionRequest{}
			name := "context_get"
			result := llm.ToolResultText("", `{"items":[{"node_id":1,"error":"cannot read"}]}`, false)
			switch kind {
			case "partial_write":
				name = "add_intent"
				result = llm.ToolResultText("", `{"ids":[1,0],"errors":[null,"cannot create"],"duplicates":[false,false]}`, false)
			case "image":
				result = llm.ToolResultText("", `{"items":[{"node_id":1}]}`, false)
				result.Content = append(result.Content, llm.ContentBlock{Type: "image"})
			case "application_json":
				name = "Bash"
				result = llm.ToolResultText("", `{"node_id":1,"ids":[2],"status":"done","secret":"application data"}`, false)
			}
			appendBudgetBatch(&req, "protected", name, result)
			for i := range 4 {
				appendBudgetBatch(&req, fmt.Sprint(i), "context_get", llm.ToolResultText("", `{"items":[]}`, false))
			}
			out := projectCompletedBatches(req)
			found := false
			for _, m := range out.Messages {
				for _, b := range m.Content {
					found = found || b.Type == llm.BlockToolUse && b.ID == "protected"
				}
			}
			if !found {
				t.Fatal("irrecoverable or partially failed batch was removed")
			}
		})
	}
}

func TestSpillReceiptRequiresRealMatchingOutputInRunDirectory(t *testing.T) {
	dir := t.TempDir()
	text := strings.Repeat("真实命令输出\n", 400)
	captured := actool.Capture(&actool.ToolContext{OutputDir: dir, MaxOutputChars: 1000}, text)
	path := verifiedSpillPointer(captured, dir)
	if path == "" {
		t.Fatal("SDK captured output not recognized", captured)
	}
	if verifiedSpillPointer(captured, t.TempDir()) != "" || verifiedSpillPointer("fake"+captured, dir) != "" {
		t.Fatal("fake/foreign pointer accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if verifiedSpillPointer(captured, dir) != "" {
		t.Fatal("missing spill file accepted")
	}
}

func TestLongContextFixedStartupMetrics(t *testing.T) {
	todos := []actool.Todo{}
	for range 100 {
		todos = append(todos, actool.Todo{Content: strings.Repeat("待", 600), Status: actool.TodoCompleted})
	}
	preview := renderPlannerTodos(todos)
	summary := strings.Repeat("证", 24000)
	raw, _ := json.Marshal(map[string]any{"summary": summary})
	intent := renderIntentTask(&db.Node{ID: 1, Payload: raw})
	t.Logf("fixed_completed_todos=100 each_chars=600 projected_chars=%d projected_bytes=%d fixed_worker_intent_chars=24000 mandate_chars=%d mandate_bytes=%d", utf8.RuneCountInString(preview), len(preview), utf8.RuneCountInString(intent), len(intent))
	if utf8.RuneCountInString(preview) > 3000 || utf8.RuneCountInString(intent) > 2100 {
		t.Fatal("startup preview exceeded budget")
	}
}
