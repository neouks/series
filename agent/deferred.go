package agent

import (
	"encoding/json"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// deferredSystem builds an agent's system-prompt segments and cache boundary from
// its DeferredInfo. When globally-available MCP tools are present, their names + a
// "prefer core tools" instruction render into a <available-deferred-tools> block
// placed as the LAST system-prompt segment, with DynamicBoundary set so the whole
// (session-fixed) system prompt — including the block — is cached (design doc
// §2.1 / C1). Skill-gated MCP names are NOT in this block; they surface when their
// skill loads. Even without a global block, the static system segment remains a
// valid cache prefix, so the boundary is always the number of returned segments.
const toolAvailabilityRule = "\n\n工具调用以本轮实际提供的名称和参数为准。正文或历史提到但本轮未提供的可选工具不代表可调用；缺少时说明限制，不猜测名称、不反复重试。延迟工具先按发现/解锁流程加载。"

const contextReadRule = "\n读取规则：搜索返回摘要和带类型引用；context_get用items:[{ref:引用}]读取，意图最新输出用view=output，摘要成员用view=members。截断按返回位置续读；未查完不能断言不存在。资产asset_search检索、asset_get按ID取详情；授权以当前角色及执行校验为准。"

func deferredSystem(sysText string, def DeferredInfo) (system []string, boundary int) {
	sysText += toolAvailabilityRule
	if def.ContextReads {
		sysText += contextReadRule
	}
	sysText += def.FindingGuidance
	sysText += def.AssetGuidance
	block := actool.RenderDeferredToolsBlock(def.GlobalNames)
	if block == "" {
		return []string{sysText}, 1
	}
	system = []string{sysText, block}
	boundary = len(system) // b >= len → whole system prompt cached (SDK guard)
	return system, boundary
}

// Appended after editable prompts so saved instructions cannot advertise an
// unbound asset reader. The handler still enforces all grants and task scope.
func plannerAssetGuidance(tools []actool.CoreTool) string {
	search, get := false, false
	for _, tool := range tools {
		search = search || tool.Name() == "asset_search"
		get = get || tool.Name() == "asset_get"
	}
	text := "\nPlanner 资产读取："
	if search {
		text += "本轮已提供 asset_search，普通检索传非空 dsl（如 ip==192.0.2.1）；仅检索本任务及直接关联任务的授权可见资产，不传 task_id。"
	} else {
		text += "本轮未提供 asset_search，不得调用；使用已提供的态势及其他实际可用工具，不猜测资产 ID。"
	}
	if get {
		text += "已知资产 ID 时按需用 asset_get 读取详情。"
	} else {
		text += "本轮未提供 asset_get，不得调用。"
	}
	return text + "工具读取权限不等于资产审批；未审批、封禁、撤回或越界方向仍须跳过，不能通过其他工具绕过。"
}

// seedUnlockFromHistory replays prior Skill() invocations in the conversation so
// their skill-gated MCPs are re-unlocked on a resumed session (design doc C2). The
// main agent builds a fresh session each turn; its in-memory unlock set would
// otherwise reset, leaving the model able to see a skill-revealed tool name yet
// unable to call it. No-op when unlockSkill is nil (no deferred tools).
func seedUnlockFromHistory(msgs []llm.Message, unlockSkill func(string)) {
	if unlockSkill == nil {
		return
	}
	// An attempted/denied/interrupted Skill call is not evidence of loading it.
	// Replay only paired successful results; otherwise a resume would unlock a
	// capability that the live execution gate had refused.
	pending := map[string]string{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse && b.Name == "Skill" && b.ID != "" {
				var in struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(b.Input, &in) == nil && in.Name != "" {
					pending[b.ID] = in.Name
				}
			}
			if b.Type == llm.BlockToolResult {
				if name, ok := pending[b.ToolUseID]; ok {
					if !b.IsError {
						unlockSkill(name)
					}
					delete(pending, b.ToolUseID)
				}
			}
		}
	}
}
