# Planner 资产搜索权限修复

## 原因与处理

旧 `list_untested_assets` 迁入 `asset_search` 时只保留 `untested` 能力，工具却展示普通 DSL 查询；已有数据库权限覆盖代码默认值，导致已绑定 Planner 的工具返回“未授予资产搜索权限”。

- 新增启动迁移标记 `tool_planner_asset_search_v1`。在旧工具迁移之后，仅为 **system、enabled 且绑定 planner** 的 `asset_search` 补齐本地 `assets` 能力。禁用、未绑定、自定义工具和其他角色不变，跨任务能力保持原值。
- 修改或重新启用该绑定时，在现有工具配置事务及锁内维护相同约束。迁移幂等；损坏的权限 JSON 使事务失败，不能标记为完成。
- 工具说明和参数按实际资产读取能力生成；普通搜索要求非空 DSL，只有未测试能力的其他角色必须传 `untested=true`。关闭覆盖度时只保留可用的普通查询，隐藏没有其他读取能力的覆盖度查询工具。
- Planner 的固定运行提示按最终工具清单追加，说明 DSL、可选详情读取、任务范围及审批限制；保留自定义提示词原文，不改写历史。
- 不修改资产审批状态、资产过滤 SQL、跨任务解析器或 Agent 调度策略，不新增表或列。

## 验证

2026-10-19，使用独立 PostgreSQL 17 临时容器运行，未连接业务数据库；报错中的 IP 仅作为存储查询夹具，没有发送网络探测。

覆盖：旧工具升级、已有不完整权限、默认配置、禁用/解绑/重新启用、自定义同名工具、幂等与错误回滚；通过真实工具装配和参数准备调用原报错 DSL，验证待审批不可见、审批后可见、撤回/封禁/删除立即隐藏、无关任务隔离、继承来源审批和非法 `task_id` 拒绝。回归同时检查其他角色权限、上下文检索、旧提示词契约、延迟调用和 Compress 的 Guard。

通过的回归命令（DSN 显式指定临时测试库）：

```sh
go test -p 1 ./db ./server ./agent ./guard -run 'Test(Context|ExplicitContext|PlannerAsset|AssetSearchPolicy|AssetReadRequiresTrusted|BuiltinProtocol|ActiveBuiltinDocumentation|StripCoverage|Deferred|InjectedGraphOverview|BuiltinToolSeeds|BuiltinSchema|TaskAssetGuard|Compress)' -count=1
go test -p 1 ./agent ./server -run 'Test(PlannerAsset|AssetSearchPolicy|StripCoverage|ContextDocumentation|BuiltinProtocol)' -count=1
```

## 生效

重新构建并重启后端后执行一次性迁移，之后新规划轮使用修正后的权限和说明。不会自动启用被禁用的工具、批准资产或重放失败调用。本次未重启业务服务、修改线上配置或提交 Git。
