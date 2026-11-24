package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

const shellDirectoryBytes = 2400
const shellHelpPageChars = 6000

func visibleShellRows(rows []*db.Tool, role string) []*db.Tool {
	out := []*db.Tool{}
	for _, r := range rows {
		if r.Kind == "shell" && r.Enabled && contains(r.Agents, role) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func shellPurpose(r *db.Tool) string {
	text := r.WhenToUse
	if strings.TrimSpace(text) == "" {
		text = strings.SplitN(r.Description, "\n", 2)[0]
	}
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	return string(runes)
}

// Only discovery metadata belongs in the permanent declaration. Keep the
// original Bash description, prompt, schema and execution wrapper intact.
func shellToolDirectory(rows []*db.Tool, role string) string {
	rows = visibleShellRows(rows, role)
	if len(rows) == 0 {
		return ""
	}
	text := "\n\n已安装命令目录（名称：用途；不是执行授权）：首次使用配置命令前，用 GetShellToolHelp({\"name\":\"名称\"}) 读取用法和约束；已有完整帮助直接复用，缺失时再读，不按执行次数重读。has_more=true 时按 next_offset 和 version 续读。目录搜索/分页用 GetShellToolHelp({\"query\":\"关键词\"})。\n"
	for i, r := range rows {
		line := "- " + r.Key + ": " + shellPurpose(r) + "\n"
		if len(text)+len(line) > shellDirectoryBytes-120 {
			text += fmt.Sprintf("还有 %d 项未展示；用 GetShellToolHelp 搜索或分页查看。", len(rows)-i)
			break
		}
		text += line
	}
	return text
}

// Called after role filtering. The helper is generated from the same authorized
// Bash surface, and can neither execute a command nor grant a capability.
func configureShellToolHelp(tools []actool.CoreTool, rows []*db.Tool, role string, current func() ([]*db.Tool, error)) []actool.CoreTool {
	bash := -1
	collision := false
	for i, t := range tools {
		if t.Name() == agent.ShellToolHelpName {
			collision = true
		} // no shadowing an existing implementation
		if t.Name() == "Bash" {
			bash = i
		}
	}
	if bash < 0 || !agent.BuiltinRoleAllows(role, "Bash") {
		return tools
	}
	if collision {
		// Preserve the old declaration if a user tool owns the helper name.
		out := append([]actool.CoreTool(nil), tools...)
		t := out[bash]
		out[bash] = agent.DecorateTool(t, t.Description()+shellToolNote(rows, role), t.InputSchema())
		return out
	}
	note := shellToolDirectory(rows, role)
	if note == "" {
		return tools
	}
	out := append([]actool.CoreTool(nil), tools...)
	t := out[bash]
	out[bash] = agent.DecorateTool(t, t.Description()+note, t.InputSchema())
	return append(out, newShellToolHelp(rows, role, current))
}

func newShellToolHelp(initial []*db.Tool, role string, current func() ([]*db.Tool, error)) actool.CoreTool {
	allowed := map[string]bool{}
	for _, r := range visibleShellRows(initial, role) {
		allowed[r.Key] = true
	}
	bashManaged := false
	for _, r := range initial {
		if r.Key == "Bash" {
			bashManaged = true
		}
	}
	return actool.Build(actool.Spec{
		Name:        agent.ShellToolHelpName,
		Description: "只读查询当前获授权的已安装命令帮助，不执行或解锁工具。name 精确读取单项完整配置；省略 name 按 query 搜索目录（空 query 列表）。offset 默认0；续页携带返回的 version。has_more=true 时继续读取；已有完整帮助直接复用。",
		Schema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"name":    map[string]any{"type": "string", "description": "已安装命令准确名称；省略则只看目录"},
			"query":   map[string]any{"type": "string", "description": "目录关键词；不能与 name 同用"},
			"offset":  map[string]any{"type": "integer", "minimum": 0, "description": "返回的 next_offset；帮助按字符，目录按条目"},
			"version": map[string]any{"type": "string", "description": "续页使用上一页的 version，防止混用已修改配置"},
		}},
		ReadOnly: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var in struct {
				Name    string `json:"name"`
				Query   string `json:"query"`
				Offset  int    `json:"offset"`
				Version string `json:"version"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return actool.Errorf("参数错误"), nil
			}
			if in.Offset < 0 || in.Offset > 10000000 || (in.Name != "" && in.Query != "") {
				return actool.Errorf("offset 越界或 name/query 冲突"), nil
			}
			if in.Offset > 0 && in.Version == "" {
				return actool.Errorf("续页必须携带 version；请从 offset=0 重新读取"), nil
			}
			if err := ctx.Err(); err != nil {
				return actool.Errorf("帮助读取已取消"), nil
			}
			rows, err := current()
			if err != nil {
				return actool.Errorf("无法核验当前工具权限，未返回帮助"), nil
			}
			bashFound := false
			for _, r := range rows {
				if r.Key == "Bash" {
					bashFound = true
					if !r.System || !r.Enabled || !contains(r.Agents, role) {
						return actool.Errorf("当前角色已无 Bash 权限"), nil
					}
				}
			}
			if !agent.BuiltinRoleAllows(role, "Bash") || (bashManaged && !bashFound) {
				return actool.Errorf("当前角色已无 Bash 权限"), nil
			}
			visible := []*db.Tool{}
			for _, r := range visibleShellRows(rows, role) {
				if allowed[r.Key] {
					visible = append(visible, r)
				}
			}
			result := map[string]any{}
			if in.Name != "" {
				var row *db.Tool
				for _, r := range visible {
					if r.Key == in.Name {
						row = r
						break
					}
				}
				if row == nil {
					return actool.Errorf("未找到当前可用命令；请省略 name，用 query 搜索获授权目录"), nil
				}
				// Preserve every configured field, including rules at the tail. Page the
				// full text rather than letting SDK output truncation silently drop it.
				body := shellToolNote([]*db.Tool{row}, role)
				hash := sha256.Sum256([]byte(body))
				version := hex.EncodeToString(hash[:])
				if in.Version != "" && in.Version != version {
					return actool.Errorf("帮助版本已变化，请从 offset=0 重新读取"), nil
				}
				chars := []rune(body)
				if in.Offset > len(chars) {
					return actool.Errorf("offset 超出帮助长度"), nil
				}
				end := min(in.Offset+shellHelpPageChars, len(chars))
				result = map[string]any{"name": row.Key, "version": version, "text": string(chars[in.Offset:end]), "total_chars": len(chars), "has_more": end < len(chars)}
				if end < len(chars) {
					result["next_offset"] = end
				}
			} else {
				items := []map[string]string{}
				q := strings.ToLower(strings.TrimSpace(in.Query))
				for _, r := range visible {
					if q == "" || strings.Contains(strings.ToLower(r.Key+" "+r.Description+" "+r.WhenToUse), q) {
						items = append(items, map[string]string{"name": r.Key, "purpose": shellPurpose(r)})
					}
				}
				fallback := len(items) == 0 && q != ""
				if fallback {
					for _, r := range visible {
						items = append(items, map[string]string{"name": r.Key, "purpose": shellPurpose(r)})
					}
				}
				data, _ := json.Marshal(items)
				hash := sha256.Sum256(append([]byte(q+"\n"), data...))
				version := hex.EncodeToString(hash[:])
				if in.Version != "" && in.Version != version {
					return actool.Errorf("目录版本已变化，请从 offset=0 重新查询"), nil
				}
				if in.Offset > len(items) {
					return actool.Errorf("offset 超出目录长度"), nil
				}
				end := min(in.Offset+10, len(items))
				result = map[string]any{"items": items[in.Offset:end], "version": version, "has_more": end < len(items), "fallback": fallback, "total": len(items)}
				if end < len(items) {
					result["next_offset"] = end
				}
			}
			encoded, _ := json.Marshal(result)
			return actool.Text(string(encoded)), nil
		},
	})
}
