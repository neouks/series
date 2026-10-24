# 导出统一为提交版报告（2026-10-24）

## 改动

- `server/server.go`：`md-single` 与 `md-report` 同样输出 `report.FindingsReportMarkdown`；`md-zip` 与 `md-report-zip` 同样输出 `buildFindingsReportZip`。导出不再调用 `StageFindingsExport`，不会在 ZIP 中打包证据报文。
- `report/findings.go`：`FindingsCSV` 列改为 `ID / 漏洞名称 / 漏洞等级 / 漏洞类型 / 状态 / 所属任务 / 发现时间 / 提交报告`；名称取提交报告里的 `**漏洞名称：**`，正文只放 `findings.report`，不再输出 summary/evidence/流量证据列。
- `server/server.go` JSON 导出：`FindingDTO.ReportDetail` 清空后再序列化，技术明细不进入下载文件。
- 前端 `findings/page.tsx`：旧的 “Markdown 汇总报告 / Markdown 分文件” 选项说明为“内容同提交版”。
- 任务详情页 `findings-tab.tsx`：“导出选中发现”从 `md-single` 改为 `md-report-zip`。
- `web/src/lib/mock/finding-export.ts`：mock 导出同步只输出 `report`，CSV/JSON 与后端一致。

## 验证

```text
go build ./server ./report ./db                     exit 0
go test -count=1 ./report                           ok
go test -count=1 ./server -run '^$'                 ok [no tests to run]
go test -count=1 ./server -run 'TestValidateFindingReport|TestSplitFindingReport'  ok
cd web && npx tsc --noEmit                          exit 0
cd web && node -r ./scripts/test-register.cjs --test src/lib/findings.test.ts  pass 3
```

数据库依赖的 `TestFindingTrafficAPIAndExport` 需要 `SERIES_PG_DSN`（或配置 `server/config.json`），本机当前未设置且未启动实例，未执行；测试代码已同步改为断言提交版导出：ZIP 不含 `evidence/` 附件、报告不泄露技术明细，CSV/JSON 不含 `report_detail`。

## 未做

- 未构建新的 `series.exe`。
- 未替换正在运行的二进制，也未重启 SERIES。
- 历史已导出的旧文件不会自动重写；重新导出即可得到提交版。

## 构建（2026-10-24）

- `web/out` 已用 `NEXT_EXPORT=1 npx next build` 重新生成，并同步到 `server/webui/dist`。
- `SERIES_SKIP_FRONTEND=1 SERIES_SKIP_NPM_CI=1 ./build.sh --target windows/amd64` 成功。
- `dist/series-windows-amd64/series.exe` SHA-256：`7bfdc4da91fb969fe0c91d070fc1676466081c8a38aa9fa68a302c4298f19f05`。
- 根目录 `series.exe` 未替换；当前运行进程仍是旧二进制，重启后新导出行为才生效。