package server

import (
	"strings"
	"testing"
)

func TestValidateFindingReportAcceptsFixedTemplate(t *testing.T) {
	report := `# XX系统存在SQL注入漏洞

**漏洞名称：** XX系统存在SQL注入漏洞
**漏洞等级：** 高危
**漏洞类型：** SQL注入
**漏洞URL：** ` + "`https://example.test/item?id=1`" + `

## 详细描述
搜索参数被直接拼接进 SQL，攻击者可改写查询条件。

## 复现步骤
1. 发送请求：

` + "```http" + `
GET /item?id=1%27 HTTP/1.1
Host: example.test
` + "```" + `

实际返回数据库错误。

## 修复方案
1. 使用参数绑定。
`
	if err := validateFindingReport(report); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
}

func TestValidateFindingReportRejectsMissingField(t *testing.T) {
	report := `# XX系统存在SQL注入漏洞

**漏洞名称：** XX系统存在SQL注入漏洞
**漏洞等级：** 高危
**漏洞类型：** SQL注入
**漏洞URL：** ` + "`https://example.test/item?id=1`" + `

## 详细描述
搜索参数被直接拼接进 SQL。

## 修复方案
1. 使用参数绑定。
`
	err := validateFindingReport(report)
	if err == nil {
		t.Fatal("report missing 复现步骤 was accepted")
	}
}

func TestValidateFindingReportRejectsStandaloneEvidenceSection(t *testing.T) {
	report := `# XX系统存在SQL注入漏洞

**漏洞名称：** XX系统存在SQL注入漏洞
**漏洞等级：** 高危
**漏洞类型：** SQL注入
**漏洞URL：** ` + "`https://example.test/item?id=1`" + `

## 详细描述
搜索参数被直接拼接进 SQL。

## 复现步骤
1. 发送请求并观察响应。

## 修复方案
1. 使用参数绑定。

## 证据
这里是另外一段证据。
`
	err := validateFindingReport(report)
	if err == nil {
		t.Fatal("standalone evidence section was accepted")
	}
}

func TestSplitFindingReportKeepsTechnicalAppendixPrivate(t *testing.T) {
	raw := "# XX系统存在SQL注入漏洞\n\n" +
		"**漏洞名称：** XX系统存在SQL注入漏洞\n" +
		"**漏洞等级：** 高危\n" +
		"**漏洞类型：** SQL注入\n" +
		"**漏洞URL：** https://example.test/item?id=1\n\n" +
		"## 详细描述\n搜索参数被直接拼接进 SQL。\n\n" +
		"## 复现步骤\n1. 发送请求并观察响应。\n\n" +
		"## 修复方案\n1. 使用参数绑定。\n\n" +
		findingReportTechnicalBegin + "\n" +
		"## 证据\n这里放完整的库表枚举、payload 变体和原始响应。\n" +
		findingReportTechnicalEnd

	if err := validateFindingReport(raw); err != nil {
		t.Fatalf("valid technical appendix rejected: %v", err)
	}
	human, detail := splitFindingReport(raw)
	if strings.Contains(human, "库表枚举") {
		t.Fatalf("technical detail leaked into submission: %s", human)
	}
	if !strings.Contains(detail, "库表枚举") || !strings.Contains(detail, findingReportTechnicalBegin) {
		t.Fatalf("technical detail was not retained: %s", detail)
	}
}

func TestValidateFindingReportRejectsInternalProcessNotes(t *testing.T) {
	report := "# XX系统存在SQL注入漏洞\n\n" +
		"**漏洞名称：** XX系统存在SQL注入漏洞\n" +
		"**漏洞等级：** 高危\n" +
		"**漏洞类型：** SQL注入\n" +
		"**漏洞URL：** https://example.test/item?id=1\n\n" +
		"## 详细描述\n搜索参数被直接拼接进 SQL。\n\n" +
		"## 复现步骤\n1. 发送请求并观察响应。\n\n" +
		"## 流量绑定说明\n本次未绑定流量。\n\n" +
		"## 修复方案\n1. 使用参数绑定。\n"

	if err := validateFindingReport(report); err == nil {
		t.Fatal("internal process note was accepted in submission")
	}
}

func TestValidateFindingReportRejectsNonNormalizedTitleAndType(t *testing.T) {
	report := "# XX系统（sqli.php 接口）存在SQL注入漏洞\n\n" +
		"**漏洞名称：** XX系统（sqli.php 接口）存在SQL注入漏洞\n" +
		"**漏洞等级：** 高危\n" +
		"**漏洞类型：** SQL注入（数字型、UNION）\n" +
		"**漏洞URL：** https://example.test/item?id=1\n\n" +
		"## 详细描述\n搜索参数被直接拼接进 SQL。\n\n" +
		"## 复现步骤\n1. 发送请求并观察响应。\n\n" +
		"## 修复方案\n1. 使用参数绑定。\n"

	if err := validateFindingReport(report); err == nil {
		t.Fatal("non-normalized title/type was accepted")
	}
}

func TestValidateFindingReportRejectsTooManySteps(t *testing.T) {
	report := "# XX系统存在SQL注入漏洞\n\n" +
		"**漏洞名称：** XX系统存在SQL注入漏洞\n" +
		"**漏洞等级：** 高危\n" +
		"**漏洞类型：** SQL注入\n" +
		"**漏洞URL：** https://example.test/item?id=1\n\n" +
		"## 详细描述\n搜索参数被直接拼接进 SQL。\n\n" +
		"## 复现步骤\n" +
		"### 步骤 1\n请求一。\n### 步骤 2\n请求二。\n### 步骤 3\n请求三。\n" +
		"### 步骤 4\n请求四。\n### 步骤 5\n请求五。\n### 步骤 6\n请求六。\n### 步骤 7\n请求七。\n\n" +
		"## 修复方案\n1. 使用参数绑定。\n"

	if err := validateFindingReport(report); err == nil {
		t.Fatal("more than six steps was accepted")
	}
}
