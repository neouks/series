package report

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/neouks/series/db"
)

// The recorder writes these footers on its legacy previews.
var packetPreviewFooter = regexp.MustCompile(`^(?:…\[truncated\] @blob sha256:[0-9a-f]{64} \(len=[0-9]+\)|\.\.\. \[截断，共 [0-9]+ 字节；完整在流量文件树\] \.\.\.)$`)

func packetNotice(label, packet string) string {
	packet = strings.TrimSpace(packet)
	if packet == "" {
		return label + "原文未记录。"
	}
	lastLine := packet[strings.LastIndexByte(packet, '\n')+1:]
	if packetPreviewFooter.MatchString(lastLine) {
		return label + "报文为截断预览，请核对已保存的流量证据。"
	}
	return ""
}

// ReproductionMarkdown renders only saved evidence, never reconstructs it.
func ReproductionMarkdown(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var reproduction db.FindingReproduction
	if err := json.Unmarshal(raw, &reproduction); err != nil {
		return "复现记录格式无效，请核对原始证据。\n"
	}
	if len(reproduction.Steps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 复现记录\n\n")
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
	for _, step := range reproduction.Steps {
		fmt.Fprintf(&b, "%s\n\n", step.Action)
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
		if step.Request != "" || step.Response != "" {
			for _, packet := range []struct{ label, text string }{{"请求", step.Request}, {"响应", step.Response}} {
				if notice := packetNotice(packet.label, packet.text); notice != "" {
					fmt.Fprintf(&b, "%s\n\n", notice)
				}
			}
		}
		fmt.Fprintf(&b, "%s\n\n", step.Result)
		if step.Proves != "" {
			fmt.Fprintf(&b, "%s\n\n", step.Proves)
		}
		if len(step.BindingIDs) > 0 {
			fmt.Fprintf(&b, "**关联流量证据编号**：%s（以当前绑定清单为准）\n\n", strings.Join(step.BindingIDs, "、"))
		}
	}
	return b.String()
}
