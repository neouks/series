package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/skill"
	actool "github.com/Autumn-27/norma/tool"
)

const toolHelpName = "GetToolHelp"

// ShellToolHelpName exposes authorized CLI documentation without executing it.
const ShellToolHelpName = "GetShellToolHelp"

// Frequency is a conservative role default, not a grant. Only tools that have
// already passed role/configuration filtering can become resident.
func coreTool(name, role string) bool {
	switch name {
	case ShellToolHelpName, "Skill", "Bash", "Read", "Write", "Edit", "MultiEdit", "LS", "Glob", "Grep", "TodoWrite",
		"context_search", "context_get", "asset_search", "graph_overview":
		return true
	}
	var names []string
	switch role {
	case "worker", "pentest", "auto":
		names = []string{"insert_assets", "record_fact", "report_finding", "traffic_search", "traffic_get"}
	case "planner":
		names = []string{"add_intent", "prove_goal", "list_task_assets"}
	case "mainagent":
		names = []string{"dispatch_intents", "add_intent", "add_hint", "set_goals", "set_constraints", "list_task_assets"}
	case "goals":
		names = []string{"set_goals", "set_constraints"}
	case "reporter":
		names = []string{"get_finding_traffic", "update_finding_report"}
	}
	return slices.Contains(names, name)
}

func compactToolText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

type residentTool struct {
	*guardedTool
	description string
}

func (t *residentTool) Description() string { return t.description }
func (*residentTool) Prompt() string        { return "" }

// NewVisibleSkillTool keeps a short applicability line for EVERY visible skill.
// The upstream listing can degrade later entries to names only at 8 KB. There
// is deliberately no aggregate cutoff here: discoverability wins over a tiny
// catalog. Full instructions and OnInvoke unlocking remain upstream-owned.
func NewVisibleSkillTool(reg *skill.Registry) actool.CoreTool {
	t := reg.Tool()
	var lines []string
	for _, s := range reg.List() {
		scenario := strings.TrimSpace(s.Description)
		if s.WhenToUse != "" && s.WhenToUse != s.Description {
			scenario = s.WhenToUse + "；" + scenario
		}
		if strings.TrimSpace(scenario) == "" {
			scenario = "未提供适用场景；按名称加载查看详细指引"
		}
		lines = append(lines, "- "+s.Name+"："+compactToolText(scenario, 220))
	}
	return &visibleSkillTool{guardedTool: &guardedTool{CoreTool: t}, listing: "可用技能（名称：适用场景）：\n" + strings.Join(lines, "\n")}
}

type visibleSkillTool struct {
	*guardedTool
	listing string
}

func (*visibleSkillTool) Description() string {
	return "根据适用场景选择技能，按名称加载完整流程；只在需要时读取正文。"
}
func (t *visibleSkillTool) Prompt() string { return t.listing }

// Help stores only docs for resident tools, so it cannot disclose a locked MCP
// or an unbound capability. Actual parameters stay in the current native schema.
func newToolHelp(docs map[string]string) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: toolHelpName, Description: "读取常驻工具的完整用法和配置说明；参数以本轮工具 Schema 为准。",
		Schema:   map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "description": "常驻工具的准确名称"}}, "required": []string{"name"}},
		ReadOnly: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var in struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return actool.Errorf("参数错误：" + err.Error()), nil
			}
			if doc, ok := docs[in.Name]; ok {
				return actool.Text(doc), nil
			}
			return actool.Errorf("未提供该常驻工具；延迟工具请使用 SearchExtraTools 发现，受限工具需先按授权流程解锁。"), nil
		},
	})
}
