package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestIntentStopToolScopesAndFailures(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("intent stop tools", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	store := d.Exploration(task.ExplorationID)
	id, err := store.AddIntent(map[string]any{"summary": "stop target"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateTask("other stop task", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(other.ID)
	foreign, err := d.Exploration(other.ExplorationID).AddIntent(map[string]any{"summary": "foreign"}, 1, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"mainagent", "planner"} {
		t.Run(role, func(t *testing.T) {
			ts := NewToolSet(store, role)
			ts.SetTaskID(task.ID)
			calls := 0
			var failure error
			stop := func(ctx context.Context, taskID, intentID int64, reason string) (IntentStopResult, error) {
				calls++
				if taskID != task.ID || intentID != id || reason != "duplicate" || RunInfoFrom(ctx).AgentKey != role {
					t.Errorf("wrong callback arguments: %d %d %q", taskID, intentID, reason)
				}
				return IntentStopResult{ID: intentID, State: "stopped", Reason: reason, CancelledByUser: role == "mainagent"}, failure
			}
			ts.killWork, ts.cancelIntent = stop, stop
			var tool actool.CoreTool = ts.killWorkTool()
			if role == "mainagent" {
				for _, candidate := range ts.MainAgentTools() {
					if candidate.Name() == "cancel_intent" {
						tool = candidate
					}
				}
			}
			good := RunInfo{TaskID: task.ID, AgentKey: role}
			input := fmt.Sprintf(`{"intent_id":"%d","reason":" duplicate "}`, id)
			for _, tc := range []struct {
				name  string
				run   RunInfo
				input string
			}{
				{"no identity", RunInfo{}, input},
				{"worker binding", RunInfo{TaskID: task.ID, AgentKey: "worker", IntentID: id}, input},
				{"other task", RunInfo{TaskID: other.ID, AgentKey: role}, input},
				{"foreign intent", good, fmt.Sprintf(`{"intent_id":%d}`, foreign)},
				{"missing id", good, `{}`},
				{"invalid id", good, `{"intent_id":-1}`},
				{"bad input", good, `{"intent_id":`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					res, err := tool.Call(WithRunInfo(t.Context(), tc.run), json.RawMessage(tc.input), nil)
					if err != nil || !res.IsError || calls != 0 {
						t.Fatalf("rejection: %s %v calls=%d", res.Flatten(), err, calls)
					}
				})
			}
			ctx := WithRunInfo(t.Context(), good)
			res, err := tool.Call(ctx, json.RawMessage(input), nil)
			var got IntentStopResult
			if err != nil || res.IsError || json.Unmarshal([]byte(res.Flatten()), &got) != nil || got.State != "stopped" || got.CancelledByUser != (role == "mainagent") || calls != 1 {
				t.Fatalf("stop: %s %v calls=%d", res.Flatten(), err, calls)
			}
			failure = errors.New("settlement failed")
			res, err = tool.Call(ctx, json.RawMessage(input), nil)
			if err != nil || !res.IsError || !strings.Contains(res.Flatten(), failure.Error()) {
				t.Fatalf("failure hidden: %s %v", res.Flatten(), err)
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			res, err = tool.Call(canceled, json.RawMessage(input), nil)
			if err != nil || !res.IsError || calls != 2 {
				t.Fatal("canceled call invoked callback", res, err, calls)
			}
		})
	}
}

func TestIntentStopCatalogAndPersistedDocumentation(t *testing.T) {
	for _, seed := range BuiltinToolSeeds() {
		want := ""
		switch seed.Key {
		case "cancel_intent":
			want = "mainagent"
		case "kill_work":
			want = "planner"
		default:
			continue
		}
		if len(seed.Agents) != 1 || seed.Agents[0] != want {
			t.Fatalf("%s roles=%v", seed.Key, seed.Agents)
		}
	}
	ts := NewToolSet(nil, "")
	for _, tool := range []actool.CoreTool{ts.cancelIntentTool(), ts.killWorkTool(), ts.steerWorkTool()} {
		resolved := ResolveBuiltinTool(tool, "旧版：仅发送信号，立即成功", nil)
		if resolved.Description() != tool.Description() {
			t.Fatal("saved metadata replaced stop contract")
		}
	}
	if !strings.Contains(mainAgentSystem("goal", "", ""), "cancel_intent") || !strings.Contains(plannerSystem("goal", "", ""), "等待中或运行中") {
		t.Fatal("missing stop guidance")
	}
}
