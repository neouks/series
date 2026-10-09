# 资产审批仅限制 Planner：实现与验证

## 实现
- 主 Agent 由可信 RunInfo（当前任务、mainagent、IntentID=0）选择执行策略；pending 可读、可操作，审批元数据仍返回 pending。
- Planner 保持 approved 门槛；主 Agent 和 Worker 仍拒绝 blocked/revoked、越界或不可用资产。普通动作 hook 不变。
- 工具、上下文动态复核、节点读取、漏洞写入和签名流量代理使用一致策略。原始对话和审计记录不改写。
- 主 Agent 下发服务在任务准入成功后，使用队列事务锁一起写入 dispatch_requested 与 pending_asset_execution（issuer/granted_at）。准入失败不写入待审批许可。
- 创建及下发操作沿用服务端 workerControlMu 串行化；任务准入与数据库写入并非跨服务的单一事务。意图创建事务和许可更新事务均沿用既有队列锁；许可发布前待审批意图不具备领取资格。
- 所有领取及启动资产检查按意图许可判定；旧意图默认无许可。切换手工模式仍清除执行选择，保留独立资产许可。
- 归档还原保留 JSON 许可；无需迁移。主 Agent 待审批产出的漏洞不向 Planner 直接推送已授权发现摘要。

## 验证结果
独立 PostgreSQL 测试库：mode_db、mode_agent、mode_server（本机测试容器，非业务库）。

| 验证 | 结果 |
|---|---|
| go test ./db | 通过，36.079s |
| go test ./server | 通过，21.551s |
| go test ./agent | 通过，16.018s |
| go test ./guard ./traffic（顺序运行） | 通过，0.311s / 2.702s |
| go test -race ./db -run TestMainPendingIntentPermissionAndClaims | 通过，1.326s |
| 服务端最后修改后的 Main/manual 下发定向回归 | 通过，0.479s |
| Agent 最后角色校验修改后的 Main/Worker 定向回归 | 通过，0.781s |
| git diff --check | 通过 |

新增回归覆盖：可信与缺失／错任务／伪造角色上下文，pending 原始状态与正文保留，Planner 正文过滤，原始请求不变，独立 hook，主 Agent 新增及重复下发、部分拒绝、准入失败、旧意图不自动放行、手工选择、并发唯一领取、撤回后拒绝及归档恢复。

测试日志位于 /tmp/artex-main-*.log。未连接真实外部模型或执行实际目标测试；本次没有前端改动，无浏览器验收。需重新构建并重启后端生效，未自动重启业务服务。
