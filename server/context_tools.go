package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strconv"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func defaultContextAccess(role string) agent.ContextAccess {
	p := db.DefaultContextGrants(role)
	return agent.ContextAccess{Local: p.Local, Cross: p.Cross}
}

type contextPolicyTool struct {
	actool.CoreTool
	policy agent.ContextAccess
	schema map[string]any
}

func (t *contextPolicyTool) InputSchema() map[string]any { return t.schema }
func (t *contextPolicyTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if err := actool.ValidateInput(t.schema, in); err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return t.CoreTool.Call(agent.WithContextAccess(ctx, t.policy), in, tc)
}
func applyContextPolicy(t actool.CoreTool, p agent.ContextAccess) actool.CoreTool {
	switch t.Name() {
	case "context_search", "context_get", "graph_overview", "asset_search", "asset_get":
	default:
		return t
	}
	b, _ := json.Marshal(t.InputSchema())
	var schema map[string]any
	_ = json.Unmarshal(b, &schema)
	props, _ := schema["properties"].(map[string]any)
	if len(p.Cross) == 0 {
		delete(props, "task_id")
	}
	if t.Name() == "context_search" {
		allowed := []string{}
		for _, kind := range agent.ContextTypes {
			if slices.Contains(p.Local, kind) || slices.Contains(p.Cross, kind) {
				allowed = append(allowed, kind)
			}
		}
		if prop, ok := props["types"].(map[string]any); ok {
			prop["items"].(map[string]any)["enum"] = allowed
		}
	}
	if t.Name() == "asset_search" && !slices.Contains(p.Local, "untested") {
		delete(props, "untested")
	}
	if t.Name() == "context_get" {
		views := []string{}
		for _, cap := range []string{"detail", "output", "members"} {
			if slices.Contains(p.Local, cap) || slices.Contains(p.Cross, cap) || (cap == "detail" && (slices.Contains(p.Local, "activity_detail") || slices.Contains(p.Cross, "activity_detail"))) {
				views = append(views, cap)
			}
		}
		if slices.Contains(p.Local, "detail") || slices.Contains(p.Cross, "detail") {
			views = append(views, "parents")
		}
		props["items"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["view"].(map[string]any)["enum"] = views
	}
	return &contextPolicyTool{CoreTool: t, policy: p, schema: schema}
}
func (s *Server) wireContextReads() {
	agent.ResolveContextTask = func(ctx context.Context, id string) (*agent.ToolSet, error) {
		task, ok := s.m.Task(id)
		if !ok {
			return nil, fmt.Errorf("任务不可用")
		}
		taskID, e := strconv.ParseInt(id, 10, 64)
		if e != nil || taskID <= 0 {
			return nil, fmt.Errorf("无效任务")
		}
		ts := agent.NewToolSet(task.Store, "orchestrator")
		ts.SetTaskID(taskID)
		if as := s.m.Assets(); as != nil {
			ts.SetAssetStore(as, as.Companies())
		}
		return ts, nil
	}
}
func loadContextAccess(pg *db.DB, role string) (agent.ContextAccess, error) {
	value, ok, err := pg.GetSetting(db.ContextCapabilitiesSetting)
	if err != nil {
		return agent.ContextAccess{}, err
	}
	if ok {
		var entries map[string]agent.ContextAccess
		if err = json.Unmarshal([]byte(value), &entries); err != nil {
			return agent.ContextAccess{}, err
		}
		if p, found := entries[role]; found {
			return p, nil
		}
	}
	return defaultContextAccess(role), nil
}

func migrateContextCatalog(pg *db.DB) error {
	if e := pg.MigrateContextTools(); e != nil {
		return e
	}
	defaults := agent.BuiltinPromptSeeds()
	defaults["reporter"] = agent.ReporterDefaultPrompt
	if err := pg.MigrateContextPrompts(defaults, agent.PreviousContextPromptHashes); err != nil {
		return err
	}
	if report, ok, err := pg.GetSetting("tool_context_prompt_migration_report"); err != nil {
		return err
	} else if ok && report != "{}" {
		log.Printf("[tools] 提示词迁移记录（自定义旧引用需人工修改）: %s", report)
	}
	return nil
}
