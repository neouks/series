package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"

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
	desc   string
}

func (t *contextPolicyTool) InputSchema() map[string]any { return t.schema }
func (t *contextPolicyTool) Description() string         { return t.desc }
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
	desc := t.Description()
	var example map[string]any
	if len(p.Cross) == 0 {
		delete(props, "task_id")
	}
	if t.Name() == "context_search" {
		allowed := []string{}
		for _, kind := range agent.ContextTypes {
			if slices.Contains(p.Local, kind) || slices.Contains(p.Cross, kind) || (kind == "activity" && (slices.Contains(p.Local, "activity_all") || slices.Contains(p.Cross, "activity_all"))) {
				allowed = append(allowed, kind)
			}
		}
		if prop, ok := props["types"].(map[string]any); ok {
			prop["items"].(map[string]any)["enum"] = allowed
			defaults := []string{}
			for _, kind := range []string{"fact", "finding", "intent"} {
				if slices.Contains(allowed, kind) {
					defaults = append(defaults, kind)
				}
			}
			prop["description"] = "可选类型：" + strings.Join(allowed, "/") + "；省略时按所选任务权限从 " + strings.Join(defaults, "/") + " 中选择；activity须显式指定"
		}
		if len(allowed) > 0 {
			kind := allowed[0]
			for _, candidate := range allowed {
				if slices.Contains(p.Local, candidate) || (candidate == "activity" && slices.Contains(p.Local, "activity_all")) {
					kind = candidate
					break
				}
			}
			if slices.Contains(p.Local, "intent") {
				kind = "intent"
			}
			example = map[string]any{"types": []string{kind}, "q": "认证"}
			if kind == "activity" && !slices.Contains(p.Local, "activity_all") {
				example["intent_id"] = 123
			}
			if !slices.Contains(p.Local, kind) && !(kind == "activity" && slices.Contains(p.Local, "activity_all")) {
				// A cross-only grant cannot support a task-local example.
				example = nil
			}
		}
	}
	if t.Name() == "asset_search" {
		assets, untested := slices.Contains(p.Local, "assets"), slices.Contains(p.Local, "untested")
		switch {
		case assets && untested:
			desc = "检索当前角色授权可见的资产摘要；普通搜索提供非空dsl，untested=true仅查询任务覆盖范围内未测试资产。"
			example = map[string]any{"dsl": "ip==192.0.2.1", "type": "ip"}
		case assets:
			delete(props, "untested")
			schema["required"] = []string{"dsl"}
			desc = "检索当前角色授权可见的资产摘要，必须提供非空dsl。当前未授予未测试资产筛选能力。"
			example = map[string]any{"dsl": "ip==192.0.2.1", "type": "ip"}
		case untested:
			// Other roles keep their migrated subset; show its actual contract.
			props["untested"] = map[string]any{"type": "boolean", "enum": []bool{true}, "description": "必须显式传true；当前仅允许查询未测试资产"}
			schema["required"] = []string{"untested"}
			desc = "仅查询当前任务覆盖范围内授权可见的未测试资产，必须显式传untested=true；dsl可选。不提供普通资产搜索。"
			example = map[string]any{"untested": true}
		default:
			desc = "当前未授予资产搜索能力，请检查工具绑定和读取权限，不要重复调用。"
		}
		if dsl, ok := props["dsl"].(map[string]any); ok {
			dsl["description"] = "筛选表达式，如 ip==192.0.2.1；支持domain/ip/url/port/technology、AND/OR、=模糊、==精确；type是独立参数，不写入dsl"
		}
		desc += " 默认页长见limit.default，最多50条；保持筛选条件用next_offset续页。任务范围及资产审批过滤仍生效，不能指定其他任务。"
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
		item := props["items"].(map[string]any)["items"].(map[string]any)
		view := item["properties"].(map[string]any)["view"].(map[string]any)
		view["enum"] = views
		labels := map[string]string{"detail": "detail：正文；activity仅此视图", "output": "output：意图最新结果/进度", "members": "members：摘要成员", "parents": "parents：意图上游事实/漏洞"}
		parts := []string{}
		for _, v := range views {
			parts = append(parts, labels[v])
		}
		view["description"] = strings.Join(parts, "；")
		if slices.Contains(views, "detail") {
			view["description"] = view["description"].(string) + "；省略默认detail"
		} else {
			item["required"] = []any{"ref", "view"}
		}
		if len(views) > 0 {
			view, ref := views[0], "node:123"
			for _, candidate := range views {
				if slices.Contains(p.Local, candidate) || (candidate == "detail" && slices.Contains(p.Local, "activity_detail")) {
					view = candidate
					break
				}
			}
			cap := view
			if cap == "detail" && !slices.Contains(p.Local, cap) && slices.Contains(p.Local, "activity_detail") {
				ref, cap = "activity:123", "activity_detail"
			}
			if slices.Contains(p.Local, cap) {
				example = map[string]any{"items": []any{map[string]any{"ref": ref, "view": view}}}
			}
		}
		delete(view, "default") // Nested defaults are not injected; do not advertise one.
		desc = "批量读取1..5个引用，按view枚举选择当前角色获准的视图。每项独立返回错误及next续读参数；顶层max_chars为总预算（缺省值见default、最多24000），每项均分，每项max_chars只能收紧，工具输出上限仍有效。"
	}
	if t.Name() == "asset_get" && slices.Contains(p.Local, "assets") {
		example = map[string]any{"ids": []int{123}}
	}
	if example != nil {
		encoded, _ := json.Marshal(example)
		desc += "\n调用示例（ID/关键词按当前上下文替换）：" + string(encoded)
	}
	return &contextPolicyTool{CoreTool: t, policy: p, schema: schema, desc: desc}
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
	if e := pg.MigratePlannerAssetSearch(); e != nil {
		return e
	}
	if e := pg.MigratePlannerIntentSearch(); e != nil {
		return e
	}
	if e := pg.MigratePlannerGraphReads(); e != nil {
		return e
	}
	defaults := agent.BuiltinPromptSeeds()
	defaults["reporter"] = agent.ReporterDefaultPrompt
	if err := pg.MigrateContextPrompts(defaults, agent.PreviousContextPromptHashes); err != nil {
		return err
	}
	if err := pg.MigrateBuiltinPromptContracts(defaults, agent.PreviousToolContractPromptHashes); err != nil {
		return err
	}
	if err := pg.MigratePlannerOnDemandPrompt(defaults["planner"], agent.PreviousToolContractPromptHashes["planner"]); err != nil {
		return err
	}
	if err := pg.MigratePlannerProactivePrompt(defaults["planner"], agent.PreviousPlannerProactivePromptHashes); err != nil {
		return err
	}
	if report, ok, err := pg.GetSetting("planner_proactive_prompt_report_v1"); err != nil {
		return err
	} else if ok && report != "{}" {
		log.Printf("[tools] Planner 主动检索提示词升级记录（自定义原文保留）: %s", report)
	}
	if report, ok, err := pg.GetSetting("tool_prompt_contracts_report_v2"); err != nil {
		return err
	} else if ok && report != "{}" {
		log.Printf("[tools] 工具契约提示词升级记录（自定义原文保留）: %s", report)
	}
	if report, ok, err := pg.GetSetting("tool_context_prompt_migration_report"); err != nil {
		return err
	} else if ok && report != "{}" {
		log.Printf("[tools] 提示词迁移记录（自定义旧引用需人工修改）: %s", report)
	}
	return nil
}

func (t *contextPolicyTool) PrepareInput(in json.RawMessage) (json.RawMessage, error) {
	return agent.PrepareToolInput(t.CoreTool, in)
}

func (t *contextPolicyTool) AcceptsRawInput() bool {
	raw, ok := t.CoreTool.(interface{ AcceptsRawInput() bool })
	return ok && raw.AcceptsRawInput()
}
