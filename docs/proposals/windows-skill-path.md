# Windows 技能路径兼容方案

## 结论

建议立即修复。这是一个明确的 Windows 回归：服务端把技能文件路径规范成反斜杠，前端再传回来时被路径安全校验拒绝，导致技能目录里的嵌套文件在 Windows 上无法正常读取/编辑。改动很小，风险低。

## 背景

SERIES 的技能管理允许用户浏览、读取、编辑 `skills/<name>/` 下的文件。前后端用相对路径传递文件位置：

- `GET /api/skills/{name}/files` 返回文件列表。
- `GET /api/skills/{name}/file?path=...` 读取文件。
- `PUT /api/skills/{name}/file` 保存文件。

路径校验在 `server/server_mgmt.go` 的 `skillRelPath`，文件遍历在 `walkSkillFiles`。

## 现状与问题

`skillRelPath` 当前用 `filepath.Clean(file)`：

```go
clean := filepath.Clean(file)
```

在 Windows 上，`filepath.Clean("scripts/run.sh")` 返回 `scripts\run.sh`。而 `skillPathRune` 的黑名单包含反斜杠：

```go
const skillPathBlocked = `\%#?*:"<>|`
```

所以：

1. `walkSkillFiles` 用 `filepath.Rel` + `filepath.WalkDir` 返回 `scripts\run.sh`。
2. 前端拿到这个路径，原样放到请求里。
3. 后端 `skillRelPath` 第一步字符检查发现 `\`，返回 `invalid path: illegal character '\\'`。
4. 结果是技能根目录文件能看，嵌套目录文件读/存失败。

这是平台差异，不是用户输入问题；Linux/macOS 上不会出现，所以容易漏测。

## 目标与非目标

**目标**

- 技能文件列表、读取、保存、删除在 Windows 上使用统一的 `/` 分隔符。
- 保留现有安全校验：拒绝 `..`、绝对路径、双斜杠、反斜杠、`%`、控制字符等。
- 不改变 Linux/macOS 行为。
- 补一个能在 Windows 上复现的回归测试。

**非目标**

- 不重构技能管理 API。
- 不改变技能目录结构。
- 不处理符号链接/挂载点等更复杂的文件系统场景（现有 `WalkDir` 行为保持不变）。

## 改造内容

### 1. `skillRelPath` 改用 slash-only 的 `path` 包

```go
import "path"

clean := path.Clean(file)
if clean == "." || path.IsAbs(clean) || strings.HasPrefix(clean, "..") {
    return "", "invalid path"
}
```

保留前面的字符白名单/黑名单检查，特别是反斜杠拒绝；因为客户端输入必须始终是 `/` 形式。

### 2. `walkSkillFiles` 输出统一为 `/`

```go
rel, relErr := filepath.Rel(root, path)
if relErr != nil || rel == "." {
    return nil
}
rel = filepath.ToSlash(rel)
```

目录仍然追加 `/`，前端用最后的 `/` 判断目录。

### 3. 调用方保持不变

`filepath.Join(skillDir, name, clean)` 仍然由操作系统处理最终路径；`clean` 始终是 `/` 形式，`filepath.Join` 在 Windows 上会正确转换。不要把 `clean` 直接拼字符串。

### 4. 测试

在 `server` 包增加表驱动测试：

- `skillRelPath("scripts/run.sh")` → `scripts/run.sh`。
- `skillRelPath("scripts\\run.sh")` → 拒绝（反斜杠仍是非法字符）。
- `skillRelPath("../secret")` → 拒绝。
- `skillRelPath("/abs")` → 拒绝。
- `skillRelPath("a//b")` → 拒绝。
- `skillRelPath("中文/脚本.sh")` → 通过。
- `walkSkillFiles` 在临时目录创建 `scripts/run.sh`，断言返回 `scripts/run.sh`，不是 `scripts\run.sh`。

如果 CI 有 Windows runner，加一个 Windows-only 集成测试；没有的话，表驱动测试也能覆盖核心逻辑。

## 为什么这么改

### 为什么用 `path` 而不是 `filepath`

`path` 包只认 `/`，与 HTTP/JSON 中的相对路径语义一致；`filepath` 会跟随操作系统，把 `/` 变成 `\`。路径校验和 API 传输层应该用 slash-only；只有最终落到文件系统时才用 `filepath.Join`。

### 为什么不在前端替换反斜杠

前端替换只能掩盖服务端输出不一致，且其他客户端（curl、脚本）仍会踩坑。服务端应该返回稳定格式。

### 为什么不放宽 `skillPathRune` 允许反斜杠

反斜杠在 Windows 是分隔符，允许它会增加路径穿越和转义歧义；保持拒绝，统一用 `/` 更安全。

### 备选方案

- 前端 `replaceAll("\\", "/")`：治标不治本，且会掩盖服务端问题。
- 全部改用绝对路径：API 暴露服务器路径，安全性和可移植性都更差。
- 只在 Windows 上特殊处理：增加分支复杂度，不如统一 slash 输出。

## 兼容性与迁移

- 已有技能文件不需要迁移；只是 API 返回格式从 `\` 变为 `/`。
- 旧前端如果缓存了 `\` 路径，刷新技能列表后恢复。
- Linux/macOS 行为不变。
- 如果外部脚本依赖 `\` 路径（不太可能），需要改为 `/`。

## 测试计划

- `go test ./server -run TestSkillRelPath`。
- `go test ./server -run TestWalkSkillFiles`。
- Windows 手工验证：在 `skills/demo/scripts/run.sh` 放置文件，打开技能编辑器，确认列表显示 `scripts/run.sh`，读取和保存成功。
- 安全回归：尝试 `..%2f`、`%252e%252e`、`C:\`、`\\server\share`，确认仍被拒绝。

## 风险与回滚

- 风险很低；主要风险是某个调用方假设了 `filepath` 分隔符。搜索 `skillRelPath` 和 `walkSkillFiles` 的所有调用点即可确认。
- 回滚：恢复 `filepath.Clean` 和原始 `rel` 输出即可，无数据变更。

## 工作量

约 0.5–1 小时代码 + 测试；Windows 手工验证 15 分钟。

## 验收标准

- Windows 上技能目录嵌套文件的列表、读取、保存全部正常。
- 反斜杠、`..`、绝对路径、双斜杠仍被拒绝。
- Linux/macOS 测试通过。
- 新增回归测试在 Windows 上能捕获旧实现的问题。

## 参考

- 韩国版提交 `02677478`：`server/server_mgmt.go` 改用 `path.Clean`，`walkSkillFiles` 用 `filepath.ToSlash`。
- SERIES 当前实现：`server/server_mgmt.go:1687`（`skillRelPath`）、`server/server_mgmt.go:1725`（`walkSkillFiles`）。