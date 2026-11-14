package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
	"github.com/neouks/series/runtimebudget"
)

func TestAddIntentRequiredSummary(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("intent summary", "goal", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	ts := NewToolSet(d.Exploration(task.ExplorationID), "planner")
	ts.SetTaskID(task.ID)
	ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: task.ID})
	add := ResolveBuiltinTool(ts.addIntent(), "", nil)
	for _, tc := range []struct {
		input, want string
		failed      bool
	}{
		{`{"summarry":"wrong field"}`, intentInputContract, true},
		{`{"summary":" \t　"}`, "summary 不能为空", true},
		{`{"intents":[{},{"summary":" "}]}`, "批量提交全部失败", true},
		{`{"intents":[{},{"summary":"valid direction"}]}`, `"ids":[0,`, false},
	} {
		r, err := add.Call(ctx, json.RawMessage(tc.input), nil)
		if err != nil || r.IsError != tc.failed || !strings.Contains(r.Flatten(), tc.want) {
			t.Fatalf("%s: is_error=%v, result=%s, err=%v", tc.input, r.IsError, r.Flatten(), err)
		}
	}
}

func TestAddIntentStopsBeforeFourthModelCall(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%v", deferred), func(t *testing.T) {
			ts := NewToolSet(&db.ExplorationStore{}, "planner")
			ts.SetTaskID(1)
			ctx := WithRunInfo(t.Context(), RunInfo{AgentKey: "planner", TaskID: 1})
			add := ResolveBuiltinTool(ts.addIntent(), "", nil)
			opts := agentcore.Options{Tools: []actool.CoreTool{add}, MaxTurns: 8,
				DisableBackgroundTasks: true, PermissionMode: permission.ModeBypass, NonStreaming: deferred}
			name, input := add.Name(), `{}`
			if deferred {
				opts.DeferredTools = []string{name}
				name, input = actool.ExecuteExtraToolName, `{"tool_name":"add_intent","params":{"intents":[{},{"summary":" "}]}}`
			}
			calls := 0
			opts.Deps = sdkTurnDeps(func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{
					Type: llm.BlockToolUse, ID: fmt.Sprint("call-", calls), Name: name, Input: json.RawMessage(input),
				}}}, "tool_use", llm.Usage{}, nil
			})
			_, _, err := captureRun(ctx, opts, "submit intent", nil)
			if calls != 3 || !runtimebudget.StopsAutomaticReplay(err) || !strings.Contains(err.Error(), "summary 不能为空") {
				t.Fatalf("model calls=%d, err=%v", calls, err)
			}
		})
	}
}
