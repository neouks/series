package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

func TestToolDocumentationHistoryKeepsOneCopyWithoutChangingAudit(t *testing.T) {
	_, docs := documentationFixture(t)
	call := func(id string) llm.Message {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "SearchExtraTools", Input: json.RawMessage(`{"query":"select:Bash"}`)}}}
	}
	result := func(id, text string, failed bool) llm.Message {
		return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, text, failed)}}
	}
	skill := llm.SkillInvocationMessage("sample", "/skills/sample", "identical instructions")
	changed := llm.SkillInvocationMessage("sample", "/skills/sample", "different caller arguments")
	req := llm.CompletionRequest{Messages: []llm.Message{call("1"), result("1", docs, false), skill, changed, call("2"), result("2", docs, false), skill, call("3"), result("3", "failed lookup", true)}}
	before, _ := json.Marshal(req)
	out := projectToolDocumentation(req)
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("mutated source history")
	}
	raw, _ := json.Marshal(out)
	for _, text := range []string{"用途说明", "identical instructions", "different caller arguments", "failed lookup"} {
		if strings.Count(string(raw), text) != 1 {
			t.Fatalf("incorrect retained copies of %q", text)
		}
	}
	if len(out.Messages) != len(req.Messages) || !reflect.DeepEqual(out, projectToolDocumentation(out)) {
		t.Fatal("projection not stable")
	}
}

func TestDeferredSkillHistoryRestoresOnlySuccessfulUnlock(t *testing.T) {
	for _, failed := range []bool{false, true} {
		messages := []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "skill", Name: "ExecuteExtraTool", Input: json.RawMessage(`{"tool_name":"Skill","params":{"name":"browser"}}`)}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("skill", "loaded", failed)}},
		}
		var unlocked []string
		seedUnlockFromHistory(messages, func(name string) { unlocked = append(unlocked, name) })
		if (!failed && !reflect.DeepEqual(unlocked, []string{"browser"})) || (failed && len(unlocked) != 0) {
			t.Fatalf("wrong unlock: %v", unlocked)
		}
	}
}

func TestDeferredSkillRecoveryIndexPreservesUnlocks(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	writer := store.NewWriter("recovery-test", "")
	for _, id := range []string{"success", "denied"} {
		input, _ := json.Marshal(map[string]any{"tool_name": "Skill", "params": map[string]string{"name": id}})
		writer.RecordMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: "ExecuteExtraTool", Input: input}}}, llm.Usage{})
		writer.RecordMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, "result", id == "denied")}}, llm.Usage{})
	}
	if err := writer.Err(); err != nil {
		t.Fatal(err)
	}
	journal, err := newRecoveryJournal(store.MainPath("recovery-test"), "recovery-test")
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if err := journal.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	var names []string
	if err := journal.ReplaySkills(t.Context(), func(name string) { names = append(names, name) }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"success"}) {
		t.Fatalf("wrong recovered unlocks: %v", names)
	}
}

func TestResidentToolHelpHistoryDeduplicatesExactResults(t *testing.T) {
	req := llm.CompletionRequest{}
	for _, id := range []string{"first", "second"} {
		req.Messages = append(req.Messages,
			llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: id, Name: toolHelpName, Input: json.RawMessage(`{"name":"Bash"}`)}}},
			llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, "original configured shell help", false)}})
	}
	out := projectToolDocumentation(req)
	raw, _ := json.Marshal(out)
	if strings.Count(string(raw), "original configured shell help") != 1 {
		t.Fatal("duplicate resident help")
	}
	if req.Messages[1].Content[0].Content[0].Text != "original configured shell help" {
		t.Fatal("mutated audit")
	}
}
