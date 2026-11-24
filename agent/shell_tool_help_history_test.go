package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestShellHelpHistoryPreservesPagesVersionsAndAudit(t *testing.T) {
	req := llm.CompletionRequest{}
	for i, item := range []struct {
		input, body string
		failed      bool
	}{
		{`{"name":"pkg"}`, "PAGE_ZERO_RULE", false},
		{`{"name":"pkg","offset":6000,"version":"v1"}`, "PAGE_ONE_RULE", false},
		{`{"name":"pkg"}`, "PAGE_ZERO_RULE", false},
		{`{"name":"pkg"}`, "CHANGED_RULE", false},
		{`{"name":"pkg"}`, "failed", true},
	} {
		id := fmt.Sprint(i)
		req.Messages = append(req.Messages, documentationCall(id, ShellToolHelpName, item.input), llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText(id, item.body, item.failed)}})
	}
	before, _ := json.Marshal(req)
	out := projectToolDocumentation(req)
	after, _ := json.Marshal(req)
	raw, _ := json.Marshal(out)
	for _, text := range []string{"PAGE_ZERO_RULE", "PAGE_ONE_RULE", "CHANGED_RULE", "failed"} {
		if strings.Count(string(raw), text) != 1 {
			t.Fatalf("lost or duplicated %s", text)
		}
	}
	if string(before) != string(after) || !reflect.DeepEqual(out, projectToolDocumentation(out)) {
		t.Fatal("audit changed or unstable projection")
	}
}

func TestShellHelpHistorySession(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, hosted := range []bool{false, true} {
			for _, noa := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v/hosted=%v/noa=%v", stream, hosted, noa), func(t *testing.T) {
					const body = "FULL_SHELL_HELP_RULE: require --read-only"
					tool := actool.Build(actool.Spec{Name: ShellToolHelpName, Description: "Read a single command help", Schema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}, "required": []string{"name"}}, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
						return actool.Text(body), nil
					}})
					tools := ConfigureBuiltinTools([]actool.CoreTool{tool}, BuiltinLoadingOptions{Role: "worker", SearchAllowed: true})
					turn := 0
					complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
						raw, _ := json.Marshal(req)
						want := 0
						if turn > 0 {
							want = 1
						}
						if strings.Count(string(raw), body) != want {
							t.Errorf("turn %d expected %d help copy", turn, want)
						}
						if turn < 2 {
							turn++
							return documentationCall(fmt.Sprint(turn), ShellToolHelpName, `{"name":"pkg"}`), "tool_use", llm.Usage{}, nil
						}
						return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
					}
					opts := agentcore.Options{Tools: tools, NonStreaming: !stream, DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, Deps: sdkTurnDeps(complete)}
					if hosted {
						opts.Transcript = transcript.NewStore(t.TempDir())
					}
					if noa {
						enableNoa(&opts, func() bool { return true }, t.TempDir(), "shell-help", func(s string) { t.Error(s) })
					}
					if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
