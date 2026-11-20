package server

import (
	"context"
	"fmt"
	"strings"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func builtinLoadingMode(pg *db.DB) string {
	mode, _, _ := pg.GetSetting(agent.BuiltinLoadingSetting)
	if mode == "direct" {
		return mode
	}
	return "on_demand"
}

func configureBuiltinLoading(ctx context.Context, pg *db.DB, role string, tools []actool.CoreTool, rows []*db.Tool) ([]actool.CoreTool, error) {
	mode, _, err := pg.GetSettingContext(ctx, agent.BuiltinLoadingSetting)
	if err != nil {
		return nil, fmt.Errorf("读取内置工具加载策略失败: %w", err)
	}
	known := agent.BuiltinToolNames()
	managed := map[string]bool{}
	custom := map[string]bool{}
	searchAllowed := false
	for _, row := range rows {
		if row.System {
			known[row.Key] = true
			managed[row.Key] = true
		} else {
			delete(known, row.Key)
			custom[row.Key] = true
		}
		if row.Key == agent.BuiltinSearchName {
			searchAllowed = row.System && row.Enabled && contains(row.Agents, role)
		}
	}
	check := func(callCtx context.Context, name string) (string, error) {
		if !agent.BuiltinRoleAllows(role, name) {
			return "unauthorized", nil
		}
		if role == "planner" && custom[name] {
			return "unauthorized", nil
		}
		if !known[name] {
			return "", nil
		} // custom/MCP retain their existing execution policy
		if err := callCtx.Err(); err != nil {
			return "unavailable", err
		}
		row, err := pg.GetTool(name)
		if err != nil {
			return "unavailable", err
		}
		if (managed[name] && row == nil) || (row != nil && (!row.System || !row.Enabled || !contains(row.Agents, role))) {
			return "unauthorized", nil
		}
		if name == "traffic_search" || name == "traffic_get" {
			enabled, err := pg.GetBoolContext(callCtx, settingTrafficCapture, false)
			if err != nil {
				return "unavailable", err
			}
			if !enabled {
				return "unavailable", nil
			}
		}
		if strings.HasPrefix(name, "shell_") {
			a, err := pg.GetAgentByKey(role)
			if err != nil {
				return "unavailable", err
			}
			if a == nil || !a.InteractiveShell {
				return "unauthorized", nil
			}
		}
		if name == "web_search" {
			a, err := pg.GetAgentByKey(role)
			if err != nil {
				return "unavailable", err
			}
			if a == nil || !a.WebSearch {
				return "unauthorized", nil
			}
			enabled, err := pg.GetBoolContext(callCtx, settingWebSearchOn, false)
			if err != nil {
				return "unavailable", err
			}
			if !enabled {
				return "unavailable", nil
			}
		}
		return "", nil
	}
	// Filter before the directory is constructed. Live checks repeat before each
	// request, discovery, approved invocation and actual registered tool execution.
	allowed := make([]actool.CoreTool, 0, len(tools))
	for _, tool := range tools {
		if !agent.BuiltinRoleAllows(role, tool.Name()) {
			continue
		}
		status, err := check(ctx, tool.Name())
		if err != nil {
			return nil, err
		}
		if status == "" {
			allowed = append(allowed, tool)
		}
	}
	return agent.ConfigureBuiltinTools(allowed, agent.BuiltinLoadingOptions{Role: role, Mode: mode, Known: known, SearchAllowed: searchAllowed, Check: check}), nil
}
