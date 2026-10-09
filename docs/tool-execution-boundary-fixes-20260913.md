# 工具执行边界修复与验收

## 范围

本轮修复深度审查确认的六项问题。不是对全部 80 个内置工具的业务行为作“零缺陷”保证，也不以少数函数的覆盖率代替全项目覆盖率。不增加工具名称别名，不改变资产审批模板或 Worker 待审批资产执行规则。

## 修复矩阵

| 问题 | 实现 | 回归 |
|---|---|---|
| 延迟调用漏掉内部权限/Hook | `ExecuteExtraTool` 必须通过运行时分派，按真实名称执行权限、Hook、审计；目标无再次分派能力 | 直接/延迟路径：允许、硬拒绝、禁用规则、只读模式、Hook 拒绝、默认参数、改参、取消、结果配对、进度及附加消息 |
| 收尾只隐藏不禁止 | `execOne` 在执行前拒绝当前收尾禁用集合，延迟目标同样检查 | 收尾拒绝且无副作用；正常阶段可用 |
| 工具 panic 逃逸 | 统一入口捕获 panic，记录服务端堆栈，返回稳定错误且不自动重试；并发元数据异常保守串行 | 权限、只读标记、前/后 Hook、Run panic；队列继续执行下一工具 |
| 工具配置读失败放行 | 装配接口显式返回错误，所有生产调用方在构造模型会话前停止并释放资源 | 关闭的 PG 连接池、五种角色、清理回调、无能力泄漏 |
| 直接调用绕过延迟解锁 | 统一入口使用会话共享解锁集合；Session 透传；历史只重放成功配对的 Skill 结果 | 锁定/解锁的直接及延迟调用、会话构建后解锁、失败/未完成历史、递归与普通工具借道 |
| 复测快照无响应预算 | 复用有界结构投影：field / index / offset / max_chars；默认 8000，完整序列化结果不超过 24000 字符 | 长中文、emoji、引号及控制字符完整续读、大数组延期、非法输入、会话隔离、DB 错误；未加载任务也读取数据库当前约束 |

原始快照及审计不截断、不迁移。复测字段路径相对于 `{"retest": ..., "current_constraints": ...}`；例如 `/retest/snapshot/finding/evidence`。返回 `value` 或 `entries`，使用条目的 `field` 展开延期字段、`next_index` 续读集合、`next_offset` 续读字符串。

## 覆盖率口径

以下八个重点函数的实测**语句覆盖率**为 100.0%：

- `harness.execOne`
- `harness.concurrencySafe`
- `tool.NewExecuteExtraTool`
- `agent.AugmentTools`
- `agent.seedUnlockFromHistory`
- `agent.parseToolDetailInput`
- `agent.ValidateToolDetailInput`
- `agent.ProjectToolDetail`

六项问题均有对应验收场景，不意味着所有包、所有分支或全部工具业务场景覆盖率为 100%。SDK 包整体覆盖率明显低于 100%，不得以本表混称。

覆盖文件保存在本机 `/tmp/artex-policy-sdk-final.cover` 与 `/tmp/artex-policy-agent-final.cover`，可用 `go tool cover -func=...` 核对。

## 验证记录与限制

- Agent 全包、独立 Server 全包、SDK harness/tool/agentcore 测试已执行。
- SDK 三包及 Agent、Server 定向 race 已执行。
- 数据库测试使用临时容器 `artex-deep-tool-policy-test`，独立端口 55439；不使用业务库 5432。
- 全项目共享库测试曾失败：并行运行出现事务/夹具状态冲突；串行运行的 `TestCoreTaskLifecyclePG` 仍出现其他测试遗留任务的后台写入与临时目录清理冲突。不能将共享库全包命令标记为通过。
- 生命周期测试已改成本地确定性模型，避免外部模型重试延迟影响三秒目标生成断言；保留生命周期业务断言。
- 最终全包编译与 `git diff --check` 通过；最新独立 Server 全包通过（42.298s），SDK 全模块 `go test -race ./...` 通过。未提交或推送代码。
- 合成长证据实测：源快照 390,039 字节，默认概览 417 字节；通过字段与偏移续读可完整还原原文。此为固定测试夹具的字节统计，不是 Token 节省比例。

诊断阶段的四个复现测试曾以“出现缺陷”为通过条件；正式回归测试现以“拒绝越界且无副作用、异常不逃逸”为通过条件，两者不能混计。
