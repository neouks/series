# Windows 技能文件路径修复 — 验证记录（2026-10-21）

对应方案：`docs/proposals/windows-skill-path.md`。

## 改动

| 文件 | 改动 |
| --- | --- |
| `server/server_mgmt.go` | `skillRelPath` 用 `path.Clean` / `path.IsAbs` 替代 `filepath.Clean` / `filepath.IsAbs`；`walkSkillFiles` 输出前 `rel = filepath.ToSlash(rel)` |
| `server/skill_upload_test.go` | `TestSkillRelPath` 补充反斜杠、UNC、盘符路径拒绝用例；新增 `TestWalkSkillFilesSlashPaths`、`TestSkillFileRoundTripNested` |
| `CHANGELOG.md` | Unreleased 新增「技能 / 修复的问题」一条 |

调用方未改动：`filepath.Join(skillDir, name, clean)` 仍由操作系统把 `/` 转成系统分隔符；反斜杠继续被 `skillPathRune` 拒绝，安全边界不变。

## 测试环境

- Windows 11，`go1.26.3 windows/amd64`，worktree `E:\AI\series\.worktrees\fix-windows-skill-path`（分支 `fix/windows-skill-path`，基于 `local-dev` @ `b79fb75`）。
- 为避免 `TestMain` 占用业务库的 advisory lock，`SERIES_PG_DSN` 指向不可达实例；所选测试全部不依赖数据库。

```
export SERIES_PG_DSN='postgres://nobody:nobody@127.0.0.1:1/none?sslmode=disable'
go test ./server -run 'TestSkillRelPath|TestWalkSkillFilesSlashPaths|TestSkillFileRoundTripNested|TestUploadSkill|TestValidSkillName' -count=1 -v
```

## 结果（修复后）

```
--- PASS: TestValidSkillName (0.00s)
--- PASS: TestSkillRelPath (0.00s)
--- PASS: TestWalkSkillFilesSlashPaths (0.01s)
--- PASS: TestSkillFileRoundTripNested (0.01s)
--- PASS: TestUploadSkillZstdAndChineseNames (0.02s)
--- PASS: TestUploadSkillGBKNames (0.01s)
--- PASS: TestUploadSkillUnsupportedMethod (0.00s)
--- PASS: TestUploadSkillEncrypted (0.00s)
--- PASS: TestUploadSkillRejectsTraversal (0.01s)
ok      github.com/neouks/series/server 6.073s
```

## 回归验证（证明测试能抓到旧实现）

把两处改动临时还原为 `filepath.Clean(file)` 并去掉 `filepath.ToSlash`，同一组测试（`TestSkillRelPath|TestWalkSkillFilesSlashPaths|TestSkillFileRoundTripNested`）失败：

```
--- FAIL: TestSkillRelPath (0.00s)
    skillRelPath("scripts/run.py") = ("scripts\\run.py", ""), want ("scripts/run.py", "")
--- FAIL: TestWalkSkillFilesSlashPaths (0.01s)
    entry "scripts\\run.sh" uses a backslash; skill paths must be / separated
    skillRelPath("scripts\\run.sh") rejected a path the server itself listed:
        invalid path: illegal character '\\'
--- FAIL: TestSkillFileRoundTripNested (0.01s)
    listed file "scripts\\run.sh" uses a backslash; must be / separated
```

第三条对应线上真实症状：列表把 `scripts\run.sh` 交给前端，前端回传后被 `skillRelPath` 判为 `illegal character '\'`，读取/保存 400。

## 覆盖到的安全回归

`TestSkillRelPath` 的拒绝表覆盖：`..` 段（含 `a/../../b`）、`/abs`、`a//b`、`scripts\run.sh`、`\\server\share\x`、`C:/abs/x`、`C:\abs\x`、`%2e%2e/x`、NUL、`#?*:"<>|`、零宽/双向控制字符、非法 UTF-8、超长路径。`TestUploadSkillRejectsTraversal`（zip-slip）照常通过。

## 未验证 / 未做

- **UI 手工验证未做**：本机 `series.exe`（8797，PID 27968）仍在跑旧二进制且接口需要鉴权，未替换、未重启；技能编辑器里展开 `scripts/` 并读写文件的确认要等重建后完成。
- **未跑全量 `go test ./...`**：其余用例需要独立 PostgreSQL。
- Linux/macOS 仅从代码路径判断行为不变（`path` 与 `filepath` 在类 Unix 上等价），未在对应平台实跑。