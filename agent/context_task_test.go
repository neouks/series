package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func TestContextTaskIDRepresentations(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`132`, "132"}, {`"132"`, "132"}, {`"00132"`, "132"}, {`""`, ""},
		{`9007199254740993`, "9007199254740993"}, // must not round through float64
		{`9223372036854775807`, "9223372036854775807"},
	} {
		var id contextTaskID
		if err := json.Unmarshal([]byte(tc.raw), &id); err != nil || string(id) != tc.want {
			t.Fatalf("%s: got %q, err=%v", tc.raw, id, err)
		}
	}
	for _, raw := range []string{`null`, `false`, `{}`, `[]`, `0`, `-132`, `132.1`, `1e2`, `"1.0"`, `"+132"`, `" 132 "`, `"abc"`, `9223372036854775808`} {
		var id contextTaskID
		if err := json.Unmarshal([]byte(raw), &id); err == nil {
			t.Fatalf("invalid ID accepted: %s", raw)
		}
	}
}

func TestContextTaskSelectorOfficialDispatch(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("context SDK selector", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	es := d.Exploration(task.ExplorationID)
	if _, err := es.AddNode(db.KindFinding, map[string]any{"summary": "SDK_CURRENT_TASK"}, 0, "confirmed", "test", nil); err != nil {
		t.Fatal(err)
	}
	ts := NewToolSet(es, "worker")
	ts.SetTaskID(task.ID)
	tool := DecorateTool(ts.contextSearch(), "", nil)
	ctx := WithContextAccess(t.Context(), ContextAccess{Local: []string{"finding"}})
	for _, stream := range []bool{false, true} {
		for _, deferred := range []bool{false, true} {
			for _, cross := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v/deferred=%v/cross=%v", stream, deferred, cross), func(t *testing.T) {
					id := task.ID
					if cross {
						id++
					}
					name := tool.Name()
					input := fmt.Sprintf(`{"task_id":%d,"types":["finding"],"limit":20}`, id)
					opts := agentcore.Options{Tools: []actool.CoreTool{tool}, PermissionMode: permission.ModeBypass, DisableBackgroundTasks: true, NonStreaming: !stream}
					if deferred {
						opts.DeferredTools = []string{tool.Name()}
						opts.UnlockSet = actool.NewUnlockSet(tool.Name())
						name, input = actool.ExecuteExtraToolName, fmt.Sprintf(`{"tool_name":%q,"params":%s}`, tool.Name(), input)
					}
					opts.Deps = sdkToolTurn(name, input)
					results := 0
					_, _, err := captureRun(ctx, opts, "read current findings", func(a db.Activity) {
						if a.Kind != "tool_result" {
							return
						}
						results++
						if a.IsError != cross || (strings.Contains(a.Detail, "SDK_CURRENT_TASK") == cross) {
							t.Errorf("unexpected result: %+v", a)
						}
					})
					if err != nil || results != 1 {
						t.Fatalf("results=%d err=%v", results, err)
					}
				})
			}
		}
	}
}

func TestContextTargetExplicitCurrentTaskUsesLocalGrants(t *testing.T) {
	ts := NewToolSet(&db.ExplorationStore{}, "worker")
	ts.SetTaskID(132)
	old := ResolveContextTask
	t.Cleanup(func() { ResolveContextTask = old })
	resolutions := 0
	remote := NewToolSet(&db.ExplorationStore{}, "test")
	ResolveContextTask = func(context.Context, string) (*ToolSet, error) {
		resolutions++
		return remote, nil
	}
	local := WithContextAccess(t.Context(), ContextAccess{Local: []string{"finding"}})
	for _, id := range []string{"", "132"} {
		got, err := ts.contextTarget(local, id, "finding")
		if err != nil || got != ts {
			t.Fatalf("current task %q: target=%p err=%v", id, got, err)
		}
	}
	if _, err := ts.contextTarget(local, "133", "finding"); err == nil || !strings.Contains(err.Error(), "跨任务") {
		t.Fatalf("foreign task should require cross grant: %v", err)
	}
	if _, err := ts.contextTarget(local, "132", "fact"); err == nil {
		t.Fatal("explicit current task bypassed resource grant")
	}
	// A model-supplied ID or RunInfo must not create a local task binding.
	unbound := NewToolSet(nil, "worker")
	if _, err := unbound.contextTarget(WithRunInfo(local, RunInfo{TaskID: 132}), "132", "finding"); err == nil {
		t.Fatal("unbound tool gained local scope")
	}
	if resolutions != 0 {
		t.Fatalf("local/denied reads resolved another store %d times", resolutions)
	}
	cross := WithContextAccess(t.Context(), ContextAccess{Cross: []string{"finding"}})
	if _, err := ts.contextTarget(cross, "132", "finding"); err == nil {
		t.Fatal("cross-only grant was used as a local grant")
	}
	if got, err := ts.contextTarget(cross, "133", "finding"); err != nil || got != remote || resolutions != 1 {
		t.Fatalf("authorized cross read changed: %p %v count=%d", got, err, resolutions)
	}
}
