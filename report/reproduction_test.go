package report

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/neouks/series/db"
)

func TestReproductionExportUsesImmutableEvidenceAndLegacyReport(t *testing.T) {
	repro := db.FindingReproduction{EntryPoint: "服务入口", Prerequisites: []string{"测试账号"}, Steps: []db.FindingReproductionStep{
		{Title: "对照", Action: "读取自己的订单", Result: "成功", Request: "GET / HTTP/1.1\r\nHost: example.test\r\n\r\n", Response: "HTTP/1.1 200 OK\r\n\r\n中文🙂", BindingIDs: []string{"42"}},
		{Title: "命令", Action: "查看输出", Result: "仅记录已知结果", Command: "printf test", Output: "```\ntest\n"},
	}}
	raw, _ := json.Marshal(repro)
	f := &db.DBFinding{ID: 123, Severity: "low", Summary: "fixture", Reproduction: raw}
	for _, rendered := range []string{SingleFindingMarkdown(f, time.Time{}), FindingsMarkdown([]*db.DBFinding{f}, time.Time{})} {
		for _, want := range []string{"## 复现记录", "读取自己的订单", "查看输出", repro.Steps[0].Request, repro.Steps[0].Response, "````\n```\ntest", "关联流量证据编号"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("missing %q: %s", want, rendered)
			}
		}
		if strings.Index(rendered, "读取自己的订单") > strings.Index(rendered, "查看输出") || strings.Contains(rendered, "### 步骤") {
			t.Fatal("expected a continuous, ordered reproduction record")
		}
	}
	f.Report = "## 原报告\n\n保持原文"
	if body := SingleFindingMarkdown(f, time.Time{}); !strings.Contains(body, f.Report) || strings.Contains(body, "## 复现记录") {
		t.Fatal("rewrote/duplicated existing report", body)
	}
}

func TestReproductionExportNotesMissingAndTruncatedPackets(t *testing.T) {
	repro := db.FindingReproduction{Steps: []db.FindingReproductionStep{{
		Action:   "核对已采集的响应",
		Response: "HTTP/1.1 200 OK\n\n预览\n…[truncated] @blob sha256:" + strings.Repeat("a", 64) + " (len=12000)",
	}}}
	raw, _ := json.Marshal(repro)
	rendered := ReproductionMarkdown(raw)
	for _, want := range []string{"请求原文未记录", "响应报文为截断预览", repro.Steps[0].Response} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %q: %s", want, rendered)
		}
	}
}
