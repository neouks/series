package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

func TestSessionPersistsLargeToolOutputAndKeepsShortOutput(t *testing.T) {
	long := strings.Repeat("build output line\n", 1800)
	short := "build passed\n"
	turn := 0
	complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		turn++
		switch turn {
		case 1:
			return documentationCall("large", "build_log", `{}`), "tool_use", llm.Usage{}, nil
		case 2:
			body := req.Messages[len(req.Messages)-1].Content[0].Content[0].Text
			if len(body) >= 15<<10 || !strings.Contains(body, "<persisted-output>") {
				t.Errorf("large output was not bounded: %d bytes", len(body))
			}
			_, path, ok := strings.Cut(body, "Full output saved to  ")
			if !ok {
				t.Error("missing full-output pointer")
			} else {
				path, _, _ = strings.Cut(path, " </persisted-output>")
				raw, err := os.ReadFile(path)
				if err != nil || string(raw) != long {
					t.Errorf("full output was not recoverable: %v", err)
				}
			}
			return documentationCall("small", "short_log", `{}`), "tool_use", llm.Usage{}, nil
		case 3:
			if got := req.Messages[len(req.Messages)-1].Content[0].Content[0].Text; got != short {
				t.Errorf("short output changed: %q", got)
			}
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
		}
		t.Errorf("unexpected turn %d", turn)
		return llm.Message{}, "end_turn", llm.Usage{}, nil
	}
	tool := func(name, body string) actool.CoreTool {
		return actool.Build(actool.Spec{Name: name, Schema: obj(nil), Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			return actool.Text(body), nil
		}})
	}
	opts := agentcore.Options{Tools: []actool.CoreTool{tool("build_log", long), tool("short_log", short)}, ToolOutputDir: t.TempDir(), DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, Deps: sdkTurnDeps(complete)}
	if _, _, err := captureRun(t.Context(), opts, "go", nil); err != nil {
		t.Fatal(err)
	}
}
