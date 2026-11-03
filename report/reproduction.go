package report

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/neouks/series/db"
)

// ReproductionMarkdown renders only saved evidence, never reconstructs it.
func ReproductionMarkdown(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var reproduction db.FindingReproduction
	if err := json.Unmarshal(raw, &reproduction); err != nil {
		return "复现记录格式无效，请核对原始证据。\n"
	}
	var b strings.Builder
	b.WriteString("## 分步复现\n\n")
	if reproduction.EntryPoint != "" {
		fmt.Fprintf(&b, "**入口**：%s\n\n", reproduction.EntryPoint)
	}
	if len(reproduction.Prerequisites) > 0 {
		b.WriteString("**前置条件**\n\n")
		for _, p := range reproduction.Prerequisites {
			fmt.Fprintf(&b, "- %s\n", p)
		}
		b.WriteString("\n")
	}
	for i, step := range reproduction.Steps {
		fmt.Fprintf(&b, "### 步骤 %d：%s\n\n%s\n\n", i+1, strings.ReplaceAll(step.Title, "\n", " "), step.Action)
		for _, block := range []struct{ label, text string }{{"请求", step.Request}, {"响应", step.Response}, {"命令", step.Command}, {"命令输出", step.Output}} {
			if block.text == "" {
				continue
			}
			// A delimiter longer than any run in the evidence keeps raw content
			// intact, even when shell output contains Markdown fences.
			fence := "```"
			for strings.Contains(block.text, fence) {
				fence += "`"
			}
			fmt.Fprintf(&b, "**%s**\n\n%s\n%s\n%s\n\n", block.label, fence, block.text, fence)
		}
		if step.Request != "" && step.Response == "" {
			b.WriteString("响应原文未记录；结论以已有证据为准。\n\n")
		}
		fmt.Fprintf(&b, "**实际结果**：%s\n\n", step.Result)
		if step.Proves != "" {
			fmt.Fprintf(&b, "**证明范围**：%s\n\n", step.Proves)
		}
		if len(step.BindingIDs) > 0 {
			fmt.Fprintf(&b, "**关联流量证据编号**：%s（以当前绑定清单为准）\n\n", strings.Join(step.BindingIDs, "、"))
		} else {
			b.WriteString("未关联流量证据；以上为现场记录。\n\n")
		}
	}
	return b.String()
}
