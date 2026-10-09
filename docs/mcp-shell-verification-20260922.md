# MCP 界面、Shell 检测及调用统计验证

## 修改
- MCP 使用紧凑卡片、搜索和自定义工具同款编辑抽屉。配置、启停、Agent 授权在抽屉内；底部固定保存、连接测试、删除。保留导入、工具发现和调用明细。
- tools 新增 executable 可选字段，启动迁移默认空字符串（使用 Key）。覆盖 HTTP CRUD、Agent 管理接口、数据库读写、前端及 Mock；从 shell 改成其他类型时清空字段。
- POST /api/tools/custom/test 支持 kind=shell、action=check/run，check 使用 key/executable/directory，run 使用 command。保留 output/is_error，增加 duration_ms。测试不保存表单、不修改工作目录、不记使用量。
- 检测先查配置目录，再查执行环境 PATH；绝对路径检查文件和执行权限。测试复用 Norma 进程管理器与输出截断，30 秒上限，取消终止进程树。Bash 与 PowerShell 分支已实现；CMD 检测返回明确不支持，手填测试仍按当前执行环境运行。Windows 实机未验证。
- Bash 装配新增语法解析计数：每请求每工具至多一次，只匹配已启用且绑定当前 Agent 的配置；实际返回失败也计数。重复归属不计数，编辑器提示配置歧义。
- 保留普通自定义工具原计数包装；直接及 ExecuteExtraTool 调用验证均无重复计数。新增 Bash 计数在既有计数器缺席时补齐，不改变工具执行结果。
- MCP 导入解析函数移到 lib/mcp-import.ts，修复 Next.js 页面导出限制导致的类型检查错误。

## 统计口径
统计“包含该工具明确调用的 Bash 执行请求”，不是实际进程次数。支持串联、管道、环境变量赋值及常见 env/sudo/command 包装；不计注释、普通字符串和函数定义；不展开动态变量、不读取脚本内容、不解析 bash -c 字符串。条件分支中的命令按提交请求计数，不推断是否实际执行。旧 Shell 历史不猜测回填。

## 验证
使用独立 PostgreSQL 测试容器 artex-sync-20260919 的 mode_db、mode_server 库，未使用业务库。

| 验证 | 结果 |
| --- | --- |
| 数据库全量 go test ./db | 通过，45.667s |
| 服务端全量 go test ./server | 通过，最终完整运行24.795s |
| 最终 Shell 接口定向回归 | 通过，0.556s |
| 最终数据库工具／统计／归档定向回归 | 通过，1.379s |
| command/script/http 直接＋延迟计数、缺少调度器拒绝 | 已包含在服务端通过测试中 |
| 前端 tsc --noEmit | 通过，包含 Next.js 生成类型 |
| 修改界面及新模块 lint（error级别） | 通过；项目样式排序等原有低级别提示未全面重排 |
| Mock 配置／测试不计数、MCP JSON 导入 | 2 项通过 |
| git diff --check | 通过 |

测试新增覆盖：目录含空格、绝对路径、目录优先于 PATH、检测不执行目标、缺失命令、无执行权限、参数误填、成功与非零退出、输出截断、请求取消／超时、旧 command 请求兼容、测试不写计数、重复命令去重、动态命令不猜测及归属冲突。

浏览器使用 NEXT_PUBLIC_MOCK=1：检查 MCP 桌面、390px 窄屏、浅色／深色、搜索、配置抽屉、连接测试、工具列表；检查 Shell 检测成功、测试失败及输入保留。浏览器验证是 Mock；实际 Shell 执行及真实 HTTP handler 使用本机测试回归验证，未连接第三方 MCP。

开发预览改用 next dev --webpack，避开既有 Turbopack 相对样式导入解析失败；未修改全局样式。Mock 保留在 http://127.0.0.1:3000/system/mcp 。测试容器用后停止。

## 截图
- MCP 桌面浅色（旧品牌截图已移除）
- MCP 窄屏深色抽屉（旧品牌截图已移除）
- Shell 测试失败回显（旧品牌截图已移除）

## 生效
重新构建前后端并重启后端；启动时自动添加 executable 列。不回填历史 Shell 计数，既有计数保留。未执行提交、推送或重启业务服务。
