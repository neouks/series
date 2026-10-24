# 报告模板改造验证记录（2026-10-23）

分支：`feat/report-template`

## 本次改动

1. `agent/promptcatalog.go`
   - `ReporterDefaultPrompt` 从旧五段式改为固定 SRC 报告模板：
     漏洞名称、漏洞等级、漏洞类型、漏洞URL、详细描述、复现步骤、影响范围（可选）、修复方案。
   - 明确要求完整请求包/响应包、中文参数 URL 编码并备注解码值、不单独写证据段、标题不塞 endpoint/参数/Payload。

2. `server/report_format.go`
   - 新增 `validateFindingReport`：检查必填段落、报告最小长度、是否仍存在独立的“证据”章节。

3. `server/orchestration.go`
   - `update_finding_report` 保存前执行结构校验。
   - 新增 `reseedReporterPrompt`：只把已知旧默认提示词升级为新模板；用户修改过的 reporter 提示词保留，不自动覆盖。
   - `update_finding_report` 工具描述同步为固定模板。

4. `agent/context_prompt_migration.go`
   - 新增 `IsLegacyReporterPrompt`，用于识别可安全升级的旧 reporter 默认提示词。

## 验证结果

- `gofmt -l agent server`：无输出
- `go test -count=1 ./server -run TestValidateFindingReport`：通过
- `go test -count=1 ./agent -run 'TestReporter|TestPrompt|TestContext'`：通过
- `go test -count=1 ./server -run '^$'`：编译通过
- 全量 `go test ./agent` 仍有既有失败：`TestCaptureApprovalLifecycle`、`TestWorkerReviewContextAcrossToolCalls`（需要 PostgreSQL）和 `TestRealCurlPicksUpUserAgent`（本机 curl exit 23），与本次改动无关。

## 当前实例状态

当前运行中的 `series.exe` 已替换为新构建（`d623ccb-dirty`，8797 健康检查正常）。当前数据库里的 reporter 提示词原本是自定义旧模板，启动迁移按“用户自定义”保留，所以 task #159《靶场测试》在 23:19–23:24 生成的 finding #1002–#1004 仍是旧格式。

2026-10-23 23:35 已手动把当前数据库的 reporter 提示词切换为新默认模板（追加 version 2，旧 version 1 保留），后续新触发的报告应使用新格式；已生成的 #1002–#1004 不会自动重写。

## 构建与替换（2026-10-23 22:57）

- 构建命令：`SERIES_SKIP_FRONTEND=1 SERIES_SKIP_NPM_CI=1 ./build.sh --target windows/amd64`
- 构建版本：`d623ccb-dirty`
- 新二进制：`E:\AI\series\series.exe`
- 新 SHA-256：`8399f12110e6f46e0b67de1356776dcc5b7ec4b5256faa5afbdb10ddbf94457e`
- 旧二进制备份：`E:\AI\series\series.exe.bak-20261023-2257`
- 旧 SHA-256：`5a3a6608a61eecf2c516e8882544a413a3c51d8c2014305c126d444347500a0c`
- 替换时未检测到运行中的 `series.exe`；用户在 22:59 启动了新进程，监听 `127.0.0.1:8797`。

## 未做

- 未重写已生成的 #1002–#1004 旧报告。
- 未把布局/质量门扩展到前端字段展示。