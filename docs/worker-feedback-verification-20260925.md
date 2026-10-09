# 主 Agent 异步下发与 Worker 回传验证

## 已实现

- 主 Agent 成功调用 `add_intent` 或 `dispatch_intents` 后，由服务端生成下发回执并结束本轮，不再调用模型等待；移除任务主 Agent 的内置 `sleep`。全部拒绝的提交仍允许模型解释失败。
- 下发许可与原主会话分段关联在同一个数据库队列事务中提交。每个会话对同一执行仅登记一次；已有运行项可订阅，明确重跑实际被领取时建立新的回传记录。
- 新增 `worker_feedback`、`worker_feedback_run`、`worker_feedback_delivery`。Worker 状态变化与回传活动原子提交；只复制最终 `result`，不把中途 `text` 当作结论。暂停及等待只报告状态。
- 回传使用独立元数据标识、原意图 ID、原始结果链接，不调用主 Agent 或 Planner，不改变主 Agent 忙碌状态。数据库 outbox 每秒按最多 100 条发布，单轮有查询超时。
- 活动 SSE 使用兼容旧数字游标的双游标（普通活动、Worker 回传），处理回传晚于其他消息推送及重连补送。前端按活动 ID 去重。
- 用户下一轮在同一主会话提问时，从数据库读取最近 20 条回传，最多 12,000 字符，不修改原始 Worker 审计或会话存档。

## 自动验证

全部使用 `127.0.0.1:55449` 的独立 `artex_feedback_*` 数据库，未使用业务数据库。

| 检查 | 结果 |
| --- | --- |
| `go test ./db -count=1` | 通过，31.502s |
| `go test ./agent -count=1` | 通过，12.192s |
| `go test ./server -count=1` | 通过，21.819s |
| 数据库 WorkerFeedback 定向 `-race` | 通过，1.878s |
| MainDispatch / DispatchReceipt / MainUserTurnLoads 定向 `-race` | 通过，1.937s |
| 服务端回传、重连及手工模式定向回归 | 通过，1.869s |
| 前端 `tsc --noEmit` | 通过 |
| activity-merge 与 worker-feedback Node 测试 | 5/5 通过 |
| `git diff --check` | 通过 |

覆盖：流式/非流式下发仅一次模型请求、工具调用与结果配对、混合成功/失败回执、并发重复登记、分段隔离、结果先落库后订阅、暂停恢复、重跑、失败无总结、事务回滚、原始结果不变、持久化恢复读取、回传不触发 Agent、较晚回传的 SSE 重连补送。

## 浏览器验证

使用 Webpack Mock，页面 `/function/tasks/detail?id=t-acme-web`：

- 原主会话同时显示已完成、已终止的 Worker 回传，明确标注意图编号。
- 回传存在时可在主 Agent 输入框输入草稿，发送按钮正常启用。
- 点击“查看 Worker 原始会话”，打开 `session=intent:101&activity=408`，正确展示对应结果并定位高亮。
- 原主会话未切换，输入草稿保留。

这是 Mock 交互验证；真实持久化、下发与 SSE 链路由独立数据库的服务端测试覆盖，没有调用外部模型或执行网络测试。

## 生效

代码未提交、未部署。重新构建并重启后端后，启动迁移自动建表；前端需更新构建。旧意图不猜测或回填来源，不会自动重放旧任务。
