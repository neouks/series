# 会话关键词搜索与来源定位验收（2026-09-21）

## 实现

- 任务会话对话新增搜索按钮及 Ctrl+F / Cmd+F。Enter / Shift+Enter 切换，Esc 关闭；工具列表、弹窗及独立聊天不接管快捷键。
- 当前会话完整正文搜索，空正文回退摘要；300ms 防抖、关键词最多 200 字符。消息按活动 ID 排序，默认 20 条、最多 50 条；游标固定搜索上界并绑定任务、会话和关键词。
- GET `/api/exploration/activity/search` 使用参数化 SQL 和 3 秒查询上下文超时，只返回 ID、类型、240 字符以内片段、下一页游标及继承来源任务标识。不返回整段历史，不计算总命中数。Token 用量记录不作为对话消息搜索。
- 历史窗口支持普通消息及工具结果；工具结果反查同会话、同 Worker 的对应命令，保留连续前后分页。继承窗口与搜索继续限制在直接来源的终态意图，排除推理及计量记录。
- 搜索定位按需读取完整正文；工具命令与结果使用各自活动 ID。CSS Highlights / DOM Range 高亮文本，不写入 HTML，不改写 Markdown、复制内容或审计数据。
- 同一消息换关键词重新居中；关闭搜索仅清除关键词高亮，返回最新恢复跟随。旧搜索及旧定位响应不能覆盖新请求。
- 无迁移，无资产授权或执行策略改动。

## 来源问题核验

使用真实后端、独立数据库 mode_browser，登记测试服务及其自动主机来源；首次工具命令前后各插入 220 条模拟活动，确保命令不在最新页。

原实现通过审批列表“来源 · 查看原始调用”进入 `session=main:0&activity=226`，历史窗口返回 200，正确展开命令和配对结果并居中。因此，该场景未复现“只切会话”，未猜测或替换来源 ID。

本次确实修复／补齐：普通消息及工具结果不能作为锚点；继承会话不支持定位窗口；旧异步分页结果可能混入新窗口；配对进一步按 Worker、意图和主 Agent 分段隔离；搜索同一消息内换关键词须重新定位段落。

## 验证结果

使用 Docker 独立测试实例 artex-sync-20260919 的 mode_db、mode_server、mode_browser，未连接业务数据库。

| 验证 | 结果 |
| --- | --- |
| DB：全文与摘要回退、中英文、字面特殊字符、固定上界、取消、普通与工具结果锚点、首次资产来源、继承访问边界 | 通过 |
| 服务端：参数限制、游标范围绑定、不同任务／会话隔离、继承与无关任务、历史窗口 | 通过 |
| 前端 `tsc --noEmit` | 通过 |
| Mock：搜索、资产来源、动作审批来源（3 项） | 通过 |
| Chrome：Ctrl+F / Cmd+F、Enter / Shift+Enter、Esc、20→40 条结果分页、未加载正文搜索、工具结果展开 | 通过 |
| Chrome：空结果、连续改词、查询失败重试、同消息长代码块换词定位、特殊字符、大小写扩展及 Emoji | 通过 |
| 真实接口：审批列表→原始具体调用→完整详情；详情失败重试；前后分页；返回筛选恢复 | 通过 |
| 真实接口：迟到结果就地显示；不存在的来源不回退；慢旧请求不覆盖新定位；手动滚动后刷新不抢位置 | 通过 |
| Chrome Mock：动作审批来源高亮兼容 | 通过 |
| 审计记录 | 浏览器只读测试前后 activity 行内容 MD5 一致 |

### 回归命令

分别设置独立测试库的 `ARTEX_PG_DSN` 后运行：

```sh
GOSUMDB=sum.golang.org go test ./db -run 'TestActivity(Search|Window|Page)|TestAssetOrigin|TestInheritedActivity' -count=1
GOSUMDB=sum.golang.org go test ./server -run 'TestActivity|TestInheritedIntentResultOnlyAllowsHistory' -count=1
cd web
npx tsc --noEmit
tsx --test src/lib/mock/activity-search.test.ts src/lib/mock/asset-origins.test.ts src/lib/mock/approval-execution.test.ts
```

### 本机截图与日志

- [搜索命中工具结果](/tmp/artex-search-check/search.png)
- [资产来源展开与高亮](/tmp/artex-search-check/source.png)
- [长代码块段落定位](/tmp/artex-search-check/long-code.png)
- [动作审批来源兼容](/tmp/artex-search-check/action-approval.png)
- 测试脚本及原始日志：`/tmp/artex-search-check/`。

重新构建前后端并重启后端后部署生效。本次仅启动并关闭独立测试服务，保留原有 3000 端口的 Mock 服务。
