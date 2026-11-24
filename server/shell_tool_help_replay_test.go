package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

// Optional read-only replay of captured requests. No model or business tools run.
func TestShellHelpRecordedRequestReplay(t *testing.T) {
	dir := os.Getenv("ARTEX_SHELL_HELP_REPLAY_DIR")
	if dir == "" {
		t.Skip("no recorded requests")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	samples := []map[string]any{}
	for _, path := range paths {
		var rec struct {
			ID  int
			Raw string `json:"raw_request"`
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal(data, &rec) != nil {
			t.Fatal("invalid record")
		}
		var req map[string]json.RawMessage
		if json.Unmarshal([]byte(rec.Raw), &req) != nil {
			t.Fatal("invalid request")
		}
		var tools []map[string]json.RawMessage
		_ = json.Unmarshal(req["tools"], &tools)
		for i, tool := range tools {
			var f struct {
				Name        string
				Description string
				Parameters  map[string]any
			}
			_ = json.Unmarshal(tool["function"], &f)
			if f.Name != "Bash" {
				continue
			}
			const marker = "\n\n以下工具已安装在当前命令执行环境中，可直接通过 Bash 工具调用：\n"
			base, body, ok := strings.Cut(f.Description, marker)
			promptAt := strings.Index(body, "\n\n"+actool.NewBash().Prompt())
			if !ok || promptAt < 0 {
				t.Fatal("unknown recorded Bash/environment boundary")
			}
			note, prompt := marker+body[:promptAt], body[promptAt+2:]
			interactiveNote := ""
			if at := strings.Index(note, "\n\n需要【交互输入】"); at >= 0 {
				note, interactiveNote = note[:at], note[at:]
			}
			rows := parseRecordedShellNote(t, note)
			original := actool.Build(actool.Spec{Name: "Bash", Description: base, Prompt: prompt, Schema: f.Parameters})
			runtime := configureShellToolHelp([]actool.CoreTool{original}, rows, "worker", func() ([]*db.Tool, error) { return rows, nil })
			if interactiveNote != "" {
				runtime[0] = agent.DecorateTool(runtime[0], runtime[0].Description()+interactiveNote, runtime[0].InputSchema())
			}
			runtime = agent.ConfigureBuiltinTools(runtime, agent.BuiltinLoadingOptions{Role: "worker", SearchAllowed: true})
			byName := names(runtime)
			bash := byName["Bash"]
			help := byName[agent.ShellToolHelpName]
			before, _ := json.Marshal(tools)
			replacement, _ := json.Marshal(map[string]any{"name": "Bash", "description": bash.Description(), "parameters": bash.InputSchema()})
			tools[i]["function"] = replacement
			helper, _ := json.Marshal(map[string]any{"name": help.Name(), "description": help.Description(), "parameters": help.InputSchema()})
			tools = append(tools, map[string]json.RawMessage{"type": json.RawMessage(`"function"`), "function": helper})
			after, _ := json.Marshal(tools)
			if len(after) >= len(before) {
				t.Fatal("declarations did not shrink")
			}
			samples = append(samples, map[string]any{"record_id": rec.ID, "commands": len(rows), "before_bash_chars": utf8.RuneCountInString(f.Description), "after_bash_chars": utf8.RuneCountInString(bash.Description()), "before_tools_bytes": len(before), "after_tools_bytes": len(after), "before_tools_chars": utf8.RuneCount(before), "after_tools_chars": utf8.RuneCount(after)})
			break
		}
	}
	if len(samples) == 0 {
		t.Fatal("no Bash records")
	}
	result, _ := json.MarshalIndent(map[string]any{"samples": samples, "method": "Offline declaration replay only; original request schemas preserved; new helper included. No provider calls or token billing measurements."}, "", "  ")
	if out := os.Getenv("ARTEX_SHELL_HELP_REPLAY_OUT"); out != "" {
		if err := os.WriteFile(out, result, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("replayed %d requests; first=%v", len(samples), samples[0])
}

// The recorder contains rendered environment metadata, not database rows.
// Invert only that exact renderer and require a lossless round trip.
func parseRecordedShellNote(t *testing.T, note string) []*db.Tool {
	t.Helper()
	const prefix = "\n\n以下工具已安装在当前命令执行环境中，可直接通过 Bash 工具调用：\n"
	if !strings.HasPrefix(note, prefix) {
		t.Fatal("unknown environment format")
	}
	body := strings.TrimPrefix(note, prefix)
	parts := strings.Split("\n"+body, "\n- ")
	rows := []*db.Tool{}
	for _, part := range parts[1:] {
		lines := strings.Split(part, "\n")
		name, desc, _ := strings.Cut(lines[0], ": ")
		row := &db.Tool{Key: name, Kind: "shell", Enabled: true, Agents: []string{"worker"}, Description: desc}
		field := &row.Description
		for _, line := range lines[1:] {
			matched := false
			for label, dest := range map[string]*string{"可执行命令": &row.Executable, "所在目录": &row.Directory, "用法帮助": &row.UsageHelp, "何时调用": &row.WhenToUse} {
				if value, ok := strings.CutPrefix(line, "  "+label+"："); ok {
					field = dest
					*field = value
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			indent := "    "
			if field == &row.Description {
				indent = "  "
			}
			value, ok := strings.CutPrefix(line, indent)
			if !ok {
				t.Fatal("invalid metadata continuation")
			}
			*field += "\n" + value
		}
		rows = append(rows, row)
	}
	if shellToolNote(rows, "worker") != note {
		t.Fatal("environment metadata did not round-trip")
	}
	return rows
}
