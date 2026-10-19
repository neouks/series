package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func TestPlannerAssetSearchMigratedCallAndScope(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })
	oldResolve, oldAugment := agent.ToolResolve, agent.ToolAugment
	t.Cleanup(func() { agent.ToolResolve, agent.ToolAugment = oldResolve, oldAugment })
	agent.ToolAugment = nil
	wireTools(pg, buildDomainReg(pg.Assets()))
	row, err := pg.GetTool("asset_search")
	if err != nil || row == nil {
		t.Fatal(row, err)
	}
	t.Cleanup(func() { pg.UpdateTool(row.Key, row.Description, row.Schema, mustJSON(row.Agents), row.Enabled) })
	for _, key := range []string{db.ContextCapabilitiesSetting, "tool_planner_asset_search_v1"} {
		value, found, err := pg.GetSetting(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if found {
				pg.SetSetting(key, value)
			} else {
				pg.Exec(`DELETE FROM settings WHERE key=$1`, key)
			}
		})
	}
	if _, err := pg.Exec(`UPDATE tools SET enabled=true,agents='["planner"]' WHERE key='asset_search'`); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetSetting(db.ContextCapabilitiesSetting, `{"planner":{"local":["untested"],"cross":[]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`DELETE FROM settings WHERE key='tool_planner_asset_search_v1'`); err != nil {
		t.Fatal(err)
	}
	newTask := func(sources ...int64) *db.Task {
		t.Helper()
		v, e := pg.CreateTaskWithOptions("Planner asset search fixture", "goal", db.TaskCreateOptions{AssetApprovalTemplate: "explicit_targets", SourceTaskIDs: sources})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { pg.DeleteTask(v.ID) })
		return v
	}
	source := newTask()
	task := newTask(source.ID)
	other := newTask()
	asset := func(taskID int64, ip string) int64 {
		t.Helper()
		id, e := pg.Assets().UpsertIP(db.UpsertIPReq{IP: ip, TaskID: taskID, AgentDiscovered: true})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { pg.Exec(`DELETE FROM assets WHERE id=$1`, id) })
		return id
	}
	ip := "123.232.119.120" // Regression uses the reported input, stored only; no network request.
	id := asset(task.ID, ip)
	outside := asset(other.ID, "192.0.2.129")
	if err := pg.Assets().ApproveTaskAssets(other.ID, []int64{outside}, "test", ""); err != nil {
		t.Fatal(err)
	}
	ts := agent.NewToolSet(pg.Exploration(task.ExplorationID), "planner")
	ts.SetTaskID(task.ID)
	ts.SetAssetStore(pg.Assets(), pg.Companies())
	ctx := agent.WithTaskToolSet(agent.WithRunInfo(t.Context(), agent.RunInfo{TaskID: task.ID, ExplorationID: task.ExplorationID, AgentKey: "planner"}), ts)
	lookup := func() actool.CoreTool {
		t.Helper()
		return names(resolveToolsForTest(t, ctx, "planner", ts.PlannerTools()))["asset_search"]
	}
	call := func(tool actool.CoreTool, raw string) actool.Result {
		t.Helper()
		in, e := agent.PrepareToolInput(tool, json.RawMessage(raw))
		if e != nil {
			t.Fatal(e)
		}
		r, e := tool.Call(ctx, in, nil)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	input := `{"dsl":"ip==123.232.119.120","limit":10}`
	if r := call(lookup(), input); !r.IsError {
		t.Fatal("legacy fixture should reject ordinary search", r.Flatten())
	}
	if err := pg.MigratePlannerAssetSearch(); err != nil {
		t.Fatal(err)
	}
	search := lookup()
	assertIDs := func(raw string, want ...int64) {
		t.Helper()
		r := call(search, raw)
		if r.IsError {
			t.Fatal(r.Flatten())
		}
		var result struct {
			Assets []struct {
				ID int64 `json:"id"`
			}
		}
		if err := json.Unmarshal([]byte(r.Flatten()), &result); err != nil {
			t.Fatal(err)
		}
		got := []int64{}
		for _, a := range result.Assets {
			got = append(got, a.ID)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("got=%v want=%v body=%s", got, want, r.Flatten())
		}
	}
	assertIDs(input) // pending stays invisible after tool-grant repair
	if err := pg.Assets().ApproveTaskAssets(task.ID, []int64{id}, "test", ""); err != nil {
		t.Fatal(err)
	}
	assertIDs(input, id)
	if err := pg.Assets().RevokeTaskAssets(task.ID, []int64{id}, "test", ""); err != nil {
		t.Fatal(err)
	}
	assertIDs(input) // same assembled tool observes the latest approval
	if err := pg.Assets().ApproveTaskAssets(task.ID, []int64{id}, "test", ""); err != nil {
		t.Fatal(err)
	}
	if err := pg.Assets().BlockTaskAssets(task.ID, []int64{id}, "test", ""); err != nil {
		t.Fatal(err)
	}
	assertIDs(input)
	assertIDs(`{"dsl":"ip==192.0.2.129"}`) // approved elsewhere is still outside scope
	if r := call(search, fmt.Sprintf(`{"dsl":"ip==192.0.2.129","task_id":%d}`, other.ID)); !r.IsError {
		t.Fatal("cross-task selector accepted", r.Flatten())
	}
	if _, err := pg.Exec(`DELETE FROM assets WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	assertIDs(input)
	// Direct source visibility still requires approval in that source task.
	inherited := asset(source.ID, "192.0.2.130")
	query := `{"dsl":"ip==192.0.2.130"}`
	assertIDs(query)
	if err := pg.Assets().ApproveTaskAssets(source.ID, []int64{inherited}, "test", ""); err != nil {
		t.Fatal(err)
	}
	assertIDs(query, inherited)
	if err := pg.Assets().RevokeTaskAssets(source.ID, []int64{inherited}, "test", ""); err != nil {
		t.Fatal(err)
	}
	assertIDs(query)
	// Disabling coverage must retain ordinary searches.
	ts.SetCoverageEnabled(false)
	search = ts.StripCoverageParams([]actool.CoreTool{lookup()})[0]
	assertIDs(input)
	if search.InputSchema()["properties"].(map[string]any)["untested"] != nil {
		t.Fatal("coverage filter still advertised")
	}
	// Binding controls remain authoritative after migration; prompts follow them.
	for _, spec := range []struct {
		enabled bool
		agents  []string
	}{{false, []string{"planner"}}, {true, []string{"worker"}}} {
		if err := pg.UpdateTool(row.Key, row.Description, row.Schema, mustJSON(spec.agents), spec.enabled); err != nil {
			t.Fatal(err)
		}
		tools, def, cleanup := augmentToolsForTest(t, ctx, "planner", ts.PlannerTools())
		cleanup()
		if names(tools)["asset_search"] != nil || !strings.Contains(def.AssetGuidance, "本轮未提供 asset_search") {
			t.Fatal("disabled binding advertised", def.AssetGuidance)
		}
	}
}

func TestAssetSearchPolicyDocumentation(t *testing.T) {
	base := names(agent.NewToolSet(nil, "planner").PlannerTools())["asset_search"]
	for _, spec := range []struct {
		grants   []string
		required string
		rejected string
	}{
		{[]string{"assets"}, "dsl", `{}`},
		{[]string{"untested"}, "untested", `{"dsl":"ip==192.0.2.1"}`},
		{[]string{"assets", "untested"}, "", `{"dsl":"ip==192.0.2.1","task_id":999}`},
	} {
		tool := applyContextPolicy(agent.ResolveBuiltinTool(base, "obsolete instructions", map[string]any{}), agent.ContextAccess{Local: spec.grants})
		if spec.required != "" && !slices.Contains(tool.InputSchema()["required"].([]string), spec.required) {
			t.Fatal(tool.InputSchema())
		}
		if strings.Contains(tool.Description(), "obsolete") || !strings.Contains(tool.Description(), "审批过滤仍生效") {
			t.Fatal(tool.Description())
		}
		r, err := tool.Call(t.Context(), json.RawMessage(spec.rejected), nil)
		if err != nil || !r.IsError {
			t.Fatal(r, err)
		}
	}
}
