# 官方 norma SDK 接入与验证（2026-10-18）

## 固定版本与恢复点

- 官方提交：[bbbb4c607966f8c03893f02d7d7d00388c8b93c8](https://github.com/Autumn-27/norma/commit/bbbb4c607966f8c03893f02d7d7d00388c8b93c8)。只使用此提交，不自动跟随 main。
- Go 版本：`v0.4.5-0.20261017151310-bbbb4c607966`。
- 模块校验：`h1:SVT+FwLwvqO0yp7UKLVHy+NzFSM/JdMW8o8YtBEsf4M=`。
- 备份引用：`codex/backup-norma-20261018-e380a76`，对应 `e380a7627543fd10e6ef15b8b6ce04066f601433`。实施前工作区干净。
- 删除本地 replace 和 SDK 副本（171 个受控文件）。`go list -m -json` 无 Replace，`go mod verify` 通过，生成二进制的模块信息也指向上述官方版本。
- `build.sh` 增加 `-mod=readonly`，构建不能悄然改动锁定依赖。历史验证文档中的本地 SDK 路径仅代表当时版本。

## 应用接入调整

1. 主 Agent、Planner、Worker、平台聊天统一经过 `agent/sdk_session.go`，使用官方 NewSession、NewBash、NewManager 和 ToolContext。
2. SERIES hook 在官方 ExecuteExtraTool 执行前解析真实目标和 params，检查最终装配集合、延迟绑定、技能解锁和权限；拒绝嵌套分发与通过分发调用普通工具。猜测延迟工具的名字直接调用也必须解锁。
3. 应用工具配置层在审批前注入标量默认值；参数改写后重新检查类型、ID／批量／预算约束以及资产策略。Worker 的 steer hook 转发资产复核，不重复执行动作审批。检索处理器本身仍校验业务边界。
4. 统计包装器只在真正进入工具执行时计数。直接／延迟调用共享同一目标包装器；保留 Bash 命令归属统计。
5. 活动记录以真实目标工具标记命令／结果，调用 ID 来自官方 ToolUseID。已验证延迟 insert_assets 登记后能指向 `main:2` 的精确命令活动；原始模型历史不改写。
6. Worker／Planner 禁用后台任务时，若启用交互 Shell，由应用单独持有官方 Manager，仅注入 shell_open/send/read/close/list，并在会话结束清理；不会因此开放 Bash 后台任务或 sleep。
7. 系统配置移除 Shell 模式选择，只保留默认解释器检测。旧 shell_mode 数据不删除、不读取；PUT 包含该字段（包括 null）返回 400，并且不部分保存其他设置。
8. 自定义工具检测、测试和模板使用平台默认解释器。保留超时、取消、输出展示与测试不计次数。Windows 旧 CMD/Git Bash/WSL 模板需要按 PowerShell 语法调整。
9. 保留资产角色授权、任务隔离、手工模式停用 Planner、主 Agent 异步下发、Worker 回传、漏洞删除反馈、检索和压缩后授权复核。没有业务数据库迁移。

## 接受的官方行为差异

| 领域 | 当前行为 |
| --- | --- |
| OpenAI 工具消息 | 官方按整段历史的 ID 集合配对。未回应调用和孤立结果会移除，但跨普通消息边界的结果仍可能被匹配，重复 ID 不再采用原本地去重规则。此类异常历史仍可能被严格网关拒绝，不能宣称已完全修复。 |
| 推理及签名 | 使用官方 reasoning_content 兜底和 thought_signature 透传。真实推理和签名保留；兜底只出现在请求中。 |
| noa | 使用官方压缩、投影、节奏和溢出策略。移除本地小窗口节奏等补丁；下述官方测试暴露的限制仍存在。平台实验开关和归档不可写时的回退保留。 |
| 输出捕获 | 使用官方 Capture / CaptureOnce，包括其截断标记识别和截断方式；不恢复本地标记强化。SERIES 的结构化授权身份保护仍在应用层。 |
| Shell | Windows 固定 PowerShell；其他平台 Bash。没有自动退回 CMD、Git Bash、WSL 或 pwsh 的选择器。 |
| 后台进程 | 使用官方 Manager 清理与 Linux 进程组内存保护默认配置。禁用后台 Manager 的 Bash 同步路径不因此获得内存守护。 |
| WebFetch 代理 | 官方代理失败后可能直连回退。此次保持该行为；全局代理覆盖和缓存刷新不在本次范围内。 |

## 验证记录

测试使用专用 PostgreSQL 17 容器、临时数据目录和本地模拟模型，未连接业务数据库。数据库相关包串行运行，避免测试包间的表清理／迁移互相干扰。

| 项目 | 结果 |
| --- | --- |
| SERIES Go 全包回归 | 通过；DB、Agent、Server、Guard、Traffic、MCP、llmrec 等均通过。生命周期测试补齐恢复任务的退出等待，避免后台会话在临时目录删除后继续写入。 |
| 接入专项 | 流式／非流式、直接／延迟、未绑定／未解锁／嵌套分发、参数默认值和改写超限、Planner 待审批拒绝、主 Agent 放行、精确来源、交互 Manager 均通过。 |
| 接入层竞态检查 | 上述分发、来源、资产策略和交互 Manager 的定向 `go test -race` 通过；宿主管理器关闭时取消并等待在途调用，拒绝关闭后启动的新调用。 |
| 模拟 OpenAI HTTP 服务 | 验证实际 JSON 的部分批次清理、孤立结果、跨边界官方行为、真实推理、签名、缺失推理兜底和用量；原始历史不变。 |
| 既有业务回归 | 主 Agent 下发后释放、多分段回传、手工模式、暂停恢复、授权变动、来源登记与历史窗口、工具计数通过。 |
| Shell 检测／测试 | 路径带空格、不可执行、缺失命令、中文和模板引用、成功／失败、取消、输出限制、不计次数通过。 |
| 编译 | Go 全包编译、Linux amd64 / macOS arm64 / Windows amd64 后端编译通过；Windows 全包测试代码交叉编译通过。没有 Windows 主机，未声称实机 PowerShell 测试通过。 |
| Linux 内存保护 | 在 1 GiB / 2 CPU 的独立 Linux 容器中执行官方测试二进制，13 项通过，无跳过；包括孤儿进程、共享页误杀保护、前台／后台终止、后代清理和上限溢出校验。 |
| 前端 | TypeScript 检查通过；48 个测试通过（含 19 个 Mock 测试）。 |
| 浏览器 | Mock 设置页桌面浅色及窄屏深色验证：Shell 卡片显示默认解释器、没有可编辑控件、没有横向溢出。界面截图已在本次对话中展示。 |
| 构建脚本 | bash 语法检查通过；模块及已构建二进制确认无本地替换。未执行发布或部署。 |

### 官方原始测试未通过项

直接在 Go 模块缓存的官方源码运行 `go test -p 2 ./...`，不修改官方源码。以下 7 项未通过，不能将官方回归描述为全绿：

| 包／测试 | 实测结果与影响 |
| --- | --- |
| llm / TestDoStreamHonorsConfiguredRetries | 测试直接调用 doStream 时 HTTPClient 为 nil，发生 panic。SERIES 的 NewProvider 会配置 HTTPClient，实际 HTTP 模拟回归通过。 |
| noa / TestAdaptiveGrowthTracksTheWindow | 默认 GrowthFloor 与 GrowthCap 都为 50,000，自适应增长区间退化。 |
| noa / TestSuppressionReleaseFitsInsideTheWindow | 小窗口的抑制解除增长量过高。 |
| noaadapter / TestLongSessionStaysBounded | 40k 窗口后十轮峰值 33,060，高于测试设定的 75% 上限。 |
| noaadapter / TestLongSessionWithPartiallyCompliantModel | 峰值 55,007，超出 40k 窗口。 |
| noaadapter / TestOverflowLearningIsWiredIn | 压力阈值仍由配置的 200k 计算，没有采用服务端报告的 128k。 |
| noaadapter / TestTierTwoIsUnreachableOnSmallWindows | 测试仍断言 tier 2 不可达，但本版本已产生 tier 2；这项属于上游断言与实现不一致。 |

明确排除上述 7 项后，llm、noa、noaadapter 其余测试通过；官方 agentcore、harness、tool、compaction、hook、permission、transcript 等其他包在原始全套运行中通过。没有通过恢复本地补丁改变这些结果。

复核命令（数据库地址必须指向独立测试库）：

```sh
go list -m -json github.com/Autumn-27/norma
go mod verify
SERIES_PG_DSN='<独立测试库 DSN>' go test -p 1 ./...
go -C "$(go list -m -f '{{.Dir}}' github.com/Autumn-27/norma)" test -p 2 ./...
cd web
npx tsc --noEmit
npx tsx --test src/lib/*.test.* src/lib/mock/*.test.*
```

## 生效与边界

需要重新构建并重启后端。此次仅修改代码与验证，没有推送、发布、修改线上配置或重启业务服务。旧会话、审计、noa 归档和业务数据未批量改写。恢复代码可参考上述备份分支，不要在有未保存改动时直接重置工作区。

## 接入复审与修复（2026-10-18）

复审保持同一个官方提交，未修改 SDK 缓存、依赖版本或恢复本地替换。以下问题均先用最小样例复现失败，再修复 SERIES 接入层。

| 级别 | 发现与复现 | 修复 |
| --- | --- | --- |
| 高 | 最终工具集合为 nil 时，官方 NewSession 自动恢复 9 个默认工具；例如只提供领域写入能力的角色在全部解绑后可能得到文件和 Bash 能力。 | `newSession` 将应用空集合显式转为空切片，阻止 SDK 的默认工具回填。显式开启的 SDK 附加能力仍遵循原配置。 |
| 高 | 资产检查在动作审批前完成；审批等待期间撤回资产、批准原命令后，未改写的参数不再复核。直接和延迟路径均复现放行。 | 动作审批返回后，无论参数是否改写，均执行最新资产复核；不重复创建动作审批，不进入实际工具计数。 |
| 中 | Worker 收尾阶段仅从 Schema 隐藏 Bash，模型继续提交该名称仍会执行。流式和非流式样例均从应执行 1 次变为执行 2 次。 | 在公开 QueryDeps 边界记录每次模型请求的工具集合，hook 拒绝调用本阶段已经隐藏的普通工具。已绑定延迟能力继续执行原有解锁／授权检查。状态依据 SDK 请求结构，不解析用户或模型文本。 |
| 高 | noa 紧急截断把结构化 JSON 切坏；审批复核解析失败后跳过处理，已撤回资产的正文片段仍进入请求。 | 仅对 SERIES 结构化结果，在 JSON 无法解析时移除正文并提供重新读取提示；保留 noa 引用、原会话和归档。Bash/MCP 普通文本和工具错误诊断不受影响。损坏的新结果不替换已有可复核成功结果。 |

相关文件：`agent/sdk_session.go`、`agent/asset_context.go`、`agent/structured_history.go`、`agent/tool_history.go`。新增回归位于 `sdk_review_test.go`、`structured_history_test.go`、`official_runtime_review_test.go`。这是权限／上下文边界修复，没有改变官方消息配对、压缩节奏、输出捕获或 Shell 算法。

### 7 项官方失败的实际影响

本次在未修改的模块缓存重新执行这 7 项，结果与首次切换一致。测量中的 token 数是官方模拟器的估算，不是付费模型实测账单。

| 失败测试 | 判断与 SERIES 中的实际影响 |
| --- | --- |
| `TestDoStreamHonorsConfiguredRetries` | 测试直接调用内部 doStream，省略 HTTPClient；不是 SERIES 正常构造路径。本次通过真实本地 HTTP 服务让 SERIES Provider 持续收到 503，流式和非流式均恰好请求 2 次后返回错误，无 panic。 |
| `TestAdaptiveGrowthTracksTheWindow` | 真实配置限制：40k 与 400k 都使用 50k 增长单元，自适应区间退化。影响压缩节奏与请求成本，不等于每个会话都会超限。 |
| `TestSuppressionReleaseFitsInsideTheWindow` | 真实小窗口风险：连续不执行／失败压缩后，需要再增长 22,500 才解除提示抑制，40k／64k 下很可能先碰到窗口上限。 |
| `TestLongSessionStaysBounded` | 样例的最后十轮峰值 33,060，为 40k 的 82.65%，违反 75% 余量目标，但该测试并没有超过 40k。属于压缩余量／成本风险，不能据此宣称模型已报上下文溢出。 |
| `TestLongSessionWithPartiallyCompliantModel` | 真实可靠性风险：只响应每三个压缩提示中的一次时，峰值 55,007 超过 40k。长会话可能被模型端拒绝；机械截断不是可靠的窗口硬上限。 |
| `TestOverflowLearningIsWiredIn` | 真实恢复限制：配置 200k、服务端报告 128k 时仍按 190k 设置恢复目标；解析函数存在但未接入学习。可能无法恢复上下文超限。SERIES 构造的不可缩减正文样例在流式／非流式各发出 1 次请求后以 `prompt_too_long` 结束，不会在此路径无限重试。官方 harness 的 reactive 重试本身也有一次限制；任务 Provider 排除确定性的上下文错误重试。 |
| `TestTierTwoIsUnreachableOnSmallWindows` | 过时的负向断言：样例实际形成 26 个 tier 1 和 2 个 tier 2 块，因此失败；不能把它解释为二级压缩失效。 |

结论：2 项没有复现为 SERIES 运行故障（内部测试构造、过时断言）；3 项是节奏／余量风险；2 项能造成长会话超限或恢复失败。后五项只在启用 noa 时适用，不能据此判定默认压缩机制同样失败。noa 默认仍为关闭，本次没有改动数据库开关。使用 noa 时必须把模型窗口配置为实际服务端容量；对 32k–64k 或频繁出现上下文超限的部署，当前可选用原有默认压缩，不能承诺官方 noa 已可靠兜底。

官方跨普通消息边界匹配工具结果的行为仍保留；异常历史可能被严格 OpenAI 兼容接口拒绝。这是上一方案明确接受的 SDK 行为差异，本次未把旧消息整理补丁搬回项目。上述修复不修改原始会话或自动重放工具。

### 本次复审验证

- 独立 PostgreSQL 17 临时容器，使用 `SERIES_PG_DSN` 显式指定测试库；测试后删除容器，未连接业务数据库。
- `go test -p 1 ./... -count=1` 全部通过：Agent 21.869s、DB 29.796s、Server 37.828s，含 Guard、Traffic、MCP、llmrec 等全部包。
- 新增定向回归全部通过：四个缺口、流式／非流式、直接／延迟审批撤回、损坏结果与旧成功结果去重、普通输出保留、HTTP 503 重试、noa 超限有界失败。
- `go test -race ./agent` 选择本次复审、官方分发、资产来源、交互 Manager、noa 授权等用例，通过（4.275s）。
- `go build ./...`、Linux amd64／Windows amd64 后端交叉编译通过；本机为 macOS arm64。未执行 Windows 实机 Shell 测试。
- `go mod verify` 与 `git diff --check` 通过；官方固定版本及无本地 replace 的约束保持不变。
- 本次只改接入层和测试／记录，没有前端改动、自动提交、发布或重启服务。
