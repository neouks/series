package agent

// Fixed guidance is derived from actual tools, so custom prompts and resumed
// conversations receive the current contract without changing stored prose.
const reproductionWriteGuidance = "\n【现场复现记录】确认漏洞时优先在 report_finding.reproduction 保存 entry_point、prerequisites 和 steps（实际顺序，不凑步数）。每步 title/action/result 讲清做什么、如何操作、实际返回什么；proves 说明证明什么。HTTP 用 request/response 原文（含真实 headers）；非 HTTP 用 command/output。缺失响应明确写未记录，不把预期当实测，不补造请求或账号。binding_ids 由服务端生成，不得传入；无包仍可登记，不为补证据重复探测。"

const reproductionReportGuidance = "\n【分步报告】优先用 context_get 读取当前漏洞 /reproduction，并按 next 续读；已有步骤是现场证据，说明可润色，请求/响应/命令/输出不可改写。只有必要信息缺失才定向查对应意图的 activity，不遍历全图。复现按“步骤N：具体操作”组织，每步先用自然语言说操作，再列真实报文或命令，最后说明实际结果及证明范围；缺失明确标注，不凑步骤、不重探测。历史漏洞没有结构化步骤时，仅依已有真实证据写 Markdown，不回填伪造现场记录。保留五部分结构及50字内修复建议，读取实际 evidence_version 后用 update_finding_report 保存。"

func reproductionSchema() map[string]any {
	step := obj(map[string]any{
		"title":   str("步骤标题，具体操作，如取得普通用户会话"),
		"action":  str("自然语言说明使用的账号/前序结果及具体操作"),
		"result":  str("实际观察到的结果；未取得响应须明确说明，不写预期冒充实测"),
		"proves":  str("本步能证明的结论及限制"),
		"request": str("真实HTTP请求原文，含headers；不改写"), "response": str("真实HTTP响应原文，含headers；缺失时省略并在result说明"),
		"command": str("真实执行命令"), "output": str("真实命令输出"),
		"traffic_refs": HintTrafficSchema(),
	}, "title", "action", "result")
	step["properties"].(map[string]any)["traffic_refs"].(map[string]any)["description"] = "可选：仅当前步骤已核实的真实流量引用；服务端转换为本漏洞的binding_ids，不猜测匹配。"
	schema := obj(map[string]any{
		"entry_point":   str("触发入口，如方法及URL或服务地址"),
		"prerequisites": map[string]any{"type": "array", "maxItems": 50, "items": str("真实的账号角色、会话及前序条件；未知明确说明")},
		"steps":         map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "items": step},
	}, "steps")
	schema["description"] = "可选：发现时记录的现场复现过程，整个JSON最多1MiB，1..50步、前置条件最多50条；长报文优先使用已核实流量引用。未提供时兼容原evidence上报。"
	return schema
}
