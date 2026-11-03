package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/db"
)

func TestReproductionReportThroughDirectAndDeferredSDK(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("reproduction tools", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(task.ID)
	ts := NewToolSet(d.Exploration(task.ExplorationID), "fixture")
	ts.SetTaskID(task.ID)
	old := FindingTrafficBindingEnabled
	t.Cleanup(func() { FindingTrafficBindingEnabled = old })
	for _, binding := range []bool{true, false} {
		FindingTrafficBindingEnabled = func() bool { return binding }
		tools, guidance := findingWorkflowTools("worker", []actool.CoreTool{ResolveBuiltinTool(ts.addFinding(), "", nil)})
		if !strings.Contains(guidance, "现场复现记录") {
			t.Fatal("missing fixed guidance")
		}
		for _, deferred := range []bool{false, true} {
			t.Run(fmt.Sprintf("binding=%v/deferred=%v", binding, deferred), func(t *testing.T) {
				name := "report_finding"
				input := `{"vulnclass":"TEST","severity":"low","summary":"中文复现","evidence":"raw evidence","reproduction":{"steps":[{"title":"查看输出","action":"核对命令","result":"已保留真实输出","command":"printf test","output":"test"}]}}`
				if deferred {
					name = actool.ExecuteExtraToolName
					input = `{"tool_name":"report_finding","params":` + input + `}`
				}
				opts := agentcore.Options{Tools: tools, PermissionMode: permission.ModeBypass, NonStreaming: true, DisableBackgroundTasks: true, Deps: sdkToolTurn(name, input)}
				if deferred {
					unlock := actool.NewUnlockSet()
					unlock.Add("report_finding")
					opts.DeferredTools = []string{"report_finding"}
					opts.UnlockSet = unlock
				}
				var result db.Activity
				_, _, err := captureRun(t.Context(), opts, "go", func(a db.Activity) {
					if a.Kind == "tool_result" {
						result = a
					}
				})
				if err != nil || result.IsError || !strings.Contains(result.Detail, "finding recorded:") {
					t.Fatal(err, result.Detail)
				}
				var recorded db.RecordedFinding
				if err = json.Unmarshal([]byte(strings.SplitN(result.Detail, "\n", 2)[1]), &recorded); err != nil {
					t.Fatal(err)
				}
				f, err := d.GetFinding(recorded.FindingID)
				if err != nil || !strings.Contains(string(f.Reproduction), "printf test") {
					t.Fatal(f, err)
				}
				d.DeleteFinding(recorded.FindingID)
			})
		}
		tool := tools[0]
		for _, raw := range []string{`"wrong"`, `{"steps":[]}`, `{"steps":[{"title":"x","action":"y","result":"z","binding_ids":["999"]}]}`} {
			res, err := tool.Call(t.Context(), json.RawMessage(`{"vulnclass":"TEST","severity":"low","summary":"bad","reproduction":`+raw+`}`), nil)
			if err != nil || !res.IsError {
				t.Fatal("bad steps registered", res, err)
			}
		}
	}
}
