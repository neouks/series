package agent

import (
	"encoding/json"
	"github.com/Autumn-27/norma/llm"
)

const evidenceNavigationRule = "\n【上下文导航与证据】信息足够可直接行动，不要求先查证明。缺信息时按资产、接口或阶段缩小范围，context_search可用navigation=true查看当前页目录。摘要、目录和经验不是原始证据；未展示或查询失败不代表不存在。possible_conflict仅提示同一显式命题的相反观测，须回读双方核对身份、条件和时间，不自动裁决或禁报。仅使用本轮实际提供的工具与参数。"

// Keep editable prose intact. Only exact copies of host-owned policies and
// byte-equivalent schemas are removed, on the ephemeral request view.
func governRequestDocs(req llm.CompletionRequest) llm.CompletionRequest {
	known := []string{evidenceNavigationRule, contextReadRule, toolAvailabilityRule, chineseLanguagePrompt}
	seen := map[string]bool{}
	system := []string{}
	removed := false
	cached := 0
	for i, text := range req.System {
		duplicate := false
		for _, rule := range known {
			if text == rule {
				duplicate = seen[rule]
				seen[rule] = true
				break
			}
		}
		if duplicate {
			removed = true
			continue
		}
		system = append(system, text)
		if i < req.DynamicBoundary {
			cached++
		}
	}
	if removed {
		req.System = system
		if req.DynamicBoundary > 0 {
			req.DynamicBoundary = cached
		}
	}
	// Normal registries have unique names: avoid serializing every schema just
	// to discover that fact on every token-generating request.
	names := map[string][]llm.ToolSchema{}
	out := []llm.ToolSchema{}
	toolRemoved := false
	for _, tool := range req.Tools {
		duplicate := false
		for _, previous := range names[tool.Name] {
			if previous.Description != tool.Description {
				continue
			}
			a, e1 := json.Marshal(previous.InputSchema)
			b, e2 := json.Marshal(tool.InputSchema)
			if e1 == nil && e2 == nil && string(a) == string(b) {
				duplicate = true
				break
			}
		}
		if duplicate {
			toolRemoved = true
			continue
		}
		names[tool.Name] = append(names[tool.Name], tool)
		out = append(out, tool)
	}
	if toolRemoved {
		req.Tools = out
	}
	return req
}
