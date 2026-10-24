package server

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The reporter writes one Markdown document. Everything outside this HTML-comment
// block is the human submission; the block itself is retained only in
// findings.report_detail for context_search / internal review. HTML comments are
// deliberate: if a stale client renders the raw document before splitting, the
// technical appendix stays invisible.
const (
	findingReportTechnicalBegin = "<!-- SERIES:TECHNICAL:BEGIN -->"
	findingReportTechnicalEnd   = "<!-- SERIES:TECHNICAL:END -->"
)

// findingReportRequiredFields are exact labels in the human submission. Requiring
// the labels (not just the words) rejects the previous free-form report shape,
// where headings like "## 漏洞名称" could pass a substring check.
var findingReportRequiredFields = []string{
	"**漏洞名称：**",
	"**漏洞等级：**",
	"**漏洞类型：**",
	"**漏洞URL：**",
	"## 详细描述",
	"## 复现步骤",
	"## 修复方案",
}

var (
	findingReportTechnicalBlock  = regexp.MustCompile(`(?s)\n?<!-- SERIES:TECHNICAL:BEGIN -->.*?<!-- SERIES:TECHNICAL:END -->\n?`)
	findingReportEvidenceHeading = regexp.MustCompile(`(?m)^#{1,3}\s*证据\s*$`)
	findingReportInternalNote    = regexp.MustCompile(`(?m)^#{1,3}\s*(流量绑定说明|克制说明)\s*$`)
	findingReportStepHeading     = regexp.MustCompile(`(?m)^(?:#{2,4}\s*)?(?:\*\*)?步骤\s*[0-9一二三四五六七八九十]`)
)

func reportLabelValue(report, label string) string {
	for _, line := range strings.Split(report, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, label) {
			return strings.TrimSpace(strings.TrimPrefix(line, label))
		}
	}
	return ""
}

func reportH1(report string) string {
	for _, line := range strings.Split(report, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

func reportSection(report, heading string) string {
	lines := strings.Split(report, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == heading {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			end = i
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}

func countSentenceEndings(text string) int {
	return strings.Count(text, "。") + strings.Count(text, "！") + strings.Count(text, "？") + strings.Count(text, "!") + strings.Count(text, "?")
}

func validateFindingReportMeta(human string) error {
	allowedLevels := map[string]bool{"严重": true, "高危": true, "中危": true, "低危": true}
	if level := reportLabelValue(human, "**漏洞等级：**"); !allowedLevels[level] {
		return fmt.Errorf("漏洞等级必须是：严重/高危/中危/低危")
	}
	url := reportLabelValue(human, "**漏洞URL：**")
	if url == "" {
		return fmt.Errorf("漏洞URL为空")
	}
	if strings.ContainsAny(url, "【】[]") {
		return fmt.Errorf("漏洞URL直接写完整 URL，不要加【】或[]")
	}
	return nil
}

func validateFindingReportTitle(human string) error {
	name := reportLabelValue(human, "**漏洞名称：**")
	if name == "" {
		return fmt.Errorf("漏洞名称为空")
	}
	if h1 := reportH1(human); h1 != "" && h1 != name {
		return fmt.Errorf("H1 标题必须与“漏洞名称”字段完全一致")
	}
	lower := strings.ToLower(name)
	if strings.Contains(lower, "http") || strings.Contains(lower, ".php") || strings.Contains(lower, ".jsp") || strings.Contains(lower, ".aspx") || strings.ContainsAny(name, "/?=【】[]") {
		return fmt.Errorf("漏洞名称不要包含接口、文件名、URL、参数或括号解释；归纳为“系统名 + 存在 + 漏洞类型 + 漏洞”")
	}
	if strings.Contains(name, "接口") || strings.Contains(name, "参数") {
		return fmt.Errorf("漏洞名称不要包含“接口/参数”；归纳为“系统名 + 存在 + 漏洞类型 + 漏洞”")
	}
	return nil
}

func validateFindingReportType(human string) error {
	vulnType := reportLabelValue(human, "**漏洞类型：**")
	if vulnType == "" {
		return fmt.Errorf("漏洞类型为空")
	}
	if strings.ContainsAny(vulnType, "（(、,，") || strings.Contains(vulnType, "+") {
		return fmt.Errorf("漏洞类型只写标准类型（例如 SQL注入），不要写数字型、UNION、盲注等利用细节")
	}
	return nil
}

func validateFindingReportBody(human string) error {
	desc := reportSection(human, "## 详细描述")
	if desc == "" {
		return fmt.Errorf("详细描述为空")
	}
	if utf8.RuneCountInString(desc) > 1200 || countSentenceEndings(desc) > 5 {
		return fmt.Errorf("详细描述只写位置、原因、影响，控制在 2–5 句；技术细节放技术附录")
	}
	steps := reportSection(human, "## 复现步骤")
	if steps == "" {
		return fmt.Errorf("复现步骤为空")
	}
	if n := len(findingReportStepHeading.FindAllStringIndex(steps, -1)); n > 6 {
		return fmt.Errorf("复现步骤最多 6 步；把备用通道和枚举细节放技术附录")
	}
	return nil
}

// splitFindingReport returns the human submission and the full technical detail.
// The detail always retains the original document; the human projection removes
// the internal technical block. Legacy reports without a block project to the
// same text, so upgrading existing findings cannot silently drop content.
func splitFindingReport(raw string) (human, detail string) {
	detail = strings.TrimSpace(raw)
	if detail == "" {
		return "", ""
	}
	human = strings.TrimSpace(findingReportTechnicalBlock.ReplaceAllString(detail, "\n"))
	if human == "" {
		// A document containing only a technical block is not a submittable
		// report. Keep it as the human fallback so validation can explain the
		// missing fields instead of replacing the content with an empty string.
		human = detail
	}
	return human, detail
}

// validateFindingReport is intentionally structural rather than semantic: it
// catches missing sections and the old standalone evidence dump without trying
// to judge whether the vulnerability is real. Evidence must live inside the
// reproduction request/response packets, not in a separate report section.
func validateFindingReport(report string) error {
	human, _ := splitFindingReport(report)
	if human == "" {
		return fmt.Errorf("报告为空")
	}
	if utf8.RuneCountInString(human) < 80 {
		return fmt.Errorf("报告过短，至少需要包含漏洞信息和可复现步骤")
	}
	var missing []string
	for _, field := range findingReportRequiredFields {
		if !strings.Contains(human, field) {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("报告缺少字段: %s", strings.Join(missing, "、"))
	}
	if findingReportEvidenceHeading.MatchString(human) {
		return fmt.Errorf("报告不要单独写“证据”段；请求包和响应包放在“复现步骤”中")
	}
	if findingReportInternalNote.MatchString(human) {
		return fmt.Errorf("报告正文不要写“流量绑定说明”或“克制说明”；技术过程请放入技术附录")
	}
	if err := validateFindingReportMeta(human); err != nil {
		return err
	}
	if err := validateFindingReportTitle(human); err != nil {
		return err
	}
	if err := validateFindingReportType(human); err != nil {
		return err
	}
	if err := validateFindingReportBody(human); err != nil {
		return err
	}
	return nil
}
