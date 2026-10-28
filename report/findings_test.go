package report

import (
	"strings"
	"testing"

	"github.com/neouks/series/db"
)

func TestSingleFindingReportMarkdownOmitsSummaryAndEvidence(t *testing.T) {
	f := &db.DBFinding{
		ID:       42,
		Name:     "旧的长标题：某接口某参数 UNION 注入",
		Severity: "critical",
		Summary:  "这是给 agent 看的摘要",
		Evidence: "这是原始证据",
		Report:   "# 某系统存在SQL注入漏洞\n\n**漏洞名称：** 某系统存在SQL注入漏洞\n\n## 详细描述\n服务端直接拼接参数。\n",
	}
	got := SingleFindingReportMarkdown(f)
	if strings.Contains(got, "这是给 agent 看的摘要") || strings.Contains(got, "这是原始证据") {
		t.Fatalf("submission export leaked internal fields: %s", got)
	}
	if !strings.Contains(got, "某系统存在SQL注入漏洞") {
		t.Fatalf("submission export lost report: %s", got)
	}
}

func TestFindingsCSVExportsOnlySubmissionReport(t *testing.T) {
	f := &db.DBFinding{
		ID:       42,
		Name:     "旧的长标题：某接口某参数 UNION 注入",
		Severity: "high",
		Summary:  "这是给 agent 看的摘要",
		Evidence: "这是原始证据",
		Report: "# 某系统存在SQL注入漏洞\n\n" +
			"**漏洞名称：** 某系统存在SQL注入漏洞\n\n" +
			"## 详细描述\n服务端直接拼接参数。\n",
	}
	got := string(FindingsCSV([]*db.DBFinding{f}))
	for _, want := range []string{"提交报告", "某系统存在SQL注入漏洞", "服务端直接拼接参数"} {
		if !strings.Contains(got, want) {
			t.Fatalf("CSV missing %q: %s", want, got)
		}
	}
	for _, leak := range []string{"这是给 agent 看的摘要", "这是原始证据", "旧的长标题"} {
		if strings.Contains(got, leak) {
			t.Fatalf("CSV leaked %q: %s", leak, got)
		}
	}
}

func TestFindingReportTitlePrefersSubmissionName(t *testing.T) {
	f := &db.DBFinding{
		ID:   42,
		Name: "某接口 sqli_id.php 的 id 参数存在 UNION SQL 注入",
		Report: "# 某系统存在SQL注入漏洞\n\n" +
			"**漏洞名称：** 某系统存在SQL注入漏洞\n",
	}
	if got := FindingReportTitle(f); got != "某系统存在SQL注入漏洞" {
		t.Fatalf("title = %q", got)
	}
	if got := FindingReportFilename(f); got != "某系统存在SQL注入漏洞.md" {
		t.Fatalf("filename = %q", got)
	}
}
