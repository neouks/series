package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/neouks/series/agent"
	"github.com/neouks/series/db"
)

func shellHelpFixture() []*db.Tool {
	return []*db.Tool{
		{Key: "Bash", System: true, Enabled: true, Agents: []string{"worker"}},
		{Key: "packager", Kind: "shell", Enabled: true, Agents: []string{"worker"}, Description: "Package artifacts", WhenToUse: "打包文件", Executable: "/opt/bin/pkg", Directory: "/opt/pkg", UsageHelp: strings.Repeat("UNUSED_LONG_HELP🙂\n", 1400) + "TAIL_RULE: require --read-only; never upload"},
		{Key: "hidden", Kind: "shell", Enabled: true, Agents: []string{"planner"}, UsageHelp: "HIDDEN_SECRET"},
	}
}

func TestShellHelpDeclarationAndFullPaging(t *testing.T) {
	rows := shellHelpFixture()
	for _, mode := range []string{"direct", "on_demand"} {
		for _, search := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/search=%v", mode, search), func(t *testing.T) {
				original := actool.NewBash()
				tools := configureShellToolHelp([]actool.CoreTool{original}, rows, "worker", func() ([]*db.Tool, error) { return rows, nil })
				tools = agent.ConfigureBuiltinTools(tools, agent.BuiltinLoadingOptions{Role: "worker", Mode: mode, SearchAllowed: search})
				tools, def := agent.OnDemandTools(tools, agent.DeferredInfo{}, "worker")
				surface := names(tools)
				bash := surface["Bash"]
				helper := surface[agent.ShellToolHelpName]
				if helper == nil || strings.Contains(bash.Description(), "UNUSED_LONG_HELP") || strings.Contains(bash.Description(), "HIDDEN_SECRET") {
					t.Fatal("eager/missing help")
				}
				if !strings.Contains(bash.Description(), original.Description()) || !strings.Contains(bash.Description(), original.Prompt()) || !reflect.DeepEqual(bash.InputSchema(), original.InputSchema()) {
					t.Fatal("changed Bash contract")
				}
				for _, name := range def.Deferred {
					if name == agent.ShellToolHelpName {
						t.Fatal("help is not directly discoverable")
					}
				}
				var combined, version string
				offset := 0
				for page := 0; page < 20; page++ {
					raw, _ := json.Marshal(map[string]any{"name": "packager", "offset": offset, "version": version})
					r, e := helper.Call(t.Context(), raw, nil)
					if e != nil || r.IsError {
						t.Fatalf("read: %v %s", e, r.Flatten())
					}
					var out struct {
						Text       string
						Version    string
						HasMore    bool `json:"has_more"`
						NextOffset int  `json:"next_offset"`
					}
					if json.Unmarshal([]byte(r.Flatten()), &out) != nil || !utf8.ValidString(out.Text) || len([]rune(out.Text)) > shellHelpPageChars {
						t.Fatal("invalid page")
					}
					combined += out.Text
					version = out.Version
					offset = out.NextOffset
					if !out.HasMore {
						break
					}
				}
				if combined != shellToolNote([]*db.Tool{rows[1]}, "worker") || !strings.Contains(combined, "TAIL_RULE") {
					t.Fatal("lost configured constraints")
				}
			})
		}
	}
}

func TestShellHelpAuthorizationAndVersion(t *testing.T) {
	rows := shellHelpFixture()
	var readErr error
	tool := newShellToolHelp(rows, "worker", func() ([]*db.Tool, error) { return rows, readErr })
	call := func(raw string, wantErr bool) string {
		t.Helper()
		r, e := tool.Call(t.Context(), json.RawMessage(raw), nil)
		if e != nil || r.IsError != wantErr {
			t.Fatalf("%s: %v %s", raw, e, r.Flatten())
		}
		return r.Flatten()
	}
	first := call(`{"name":"packager"}`, false)
	var out map[string]any
	_ = json.Unmarshal([]byte(first), &out)
	call(`{"name":"hidden"}`, true)
	call(`{"name":"packager","offset":1}`, true)
	call(`{"name":"packager","query":"x"}`, true)
	call(`{"offset":-1}`, true)
	call(`{"offset":0.5}`, true)
	rows[1].UsageHelp += "changed"
	raw, _ := json.Marshal(map[string]any{"name": "packager", "version": out["version"], "offset": 6000})
	call(string(raw), true)
	rows[1].Enabled = false
	call(`{"name":"packager"}`, true)
	if strings.Contains(call(`{}`, false), "packager") {
		t.Fatal("disabled directory leak")
	}
	rows[1].Enabled = true
	rows[1].Agents = []string{"planner"}
	call(`{"name":"packager"}`, true)
	rows[1].Agents = []string{"worker"}
	rows[0].Enabled = false
	call(`{}`, true)
	rows[0].Enabled = true
	readErr = errors.New("db unavailable")
	call(`{}`, true)
	readErr = nil
	rows = rows[1:]
	call(`{}`, true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r, _ := tool.Call(ctx, json.RawMessage(`{}`), nil)
	if !r.IsError {
		t.Fatal("ignored cancellation")
	}
	for _, base := range [][]actool.CoreTool{nil, {actool.NewRead()}} {
		if len(configureShellToolHelp(base, rows, "worker", nil)) != len(base) {
			t.Fatal("helper without Bash")
		}
	}
	if len(configureShellToolHelp([]actool.CoreTool{actool.NewBash()}, shellHelpFixture(), "planner", nil)) != 1 {
		t.Fatal("Planner helper exposure")
	}
}

func TestShellHelpBoundedDirectoryAndFallback(t *testing.T) {
	rows := shellHelpFixture()
	for i := 0; i < 150; i++ {
		rows = append(rows, &db.Tool{Key: fmt.Sprintf("cmd-%03d", i), Kind: "shell", Enabled: true, Agents: []string{"worker"}, WhenToUse: "用途" + strings.Repeat("说明", 200), UsageHelp: "PRIVATE_LONG_BODY"})
	}
	directory := shellToolDirectory(rows, "worker")
	if len(directory) > shellDirectoryBytes || !strings.Contains(directory, "未展示") || strings.Contains(directory, "PRIVATE_LONG_BODY") {
		t.Fatal("unbounded directory")
	}
	tool := newShellToolHelp(rows, "worker", func() ([]*db.Tool, error) { return rows, nil })
	r, _ := tool.Call(t.Context(), json.RawMessage(`{"query":"cmd-149"}`), nil)
	if r.IsError || !strings.Contains(r.Flatten(), "cmd-149") || strings.Contains(r.Flatten(), "PRIVATE_LONG_BODY") {
		t.Fatal("hidden name cannot be discovered")
	}
	r, _ = tool.Call(t.Context(), json.RawMessage(`{"query":"no-such-command"}`), nil)
	if !strings.Contains(r.Flatten(), `"fallback":true`) || !strings.Contains(r.Flatten(), `"has_more":true`) {
		t.Fatal("missing fallback")
	}
	var page map[string]any
	_ = json.Unmarshal([]byte(r.Flatten()), &page)
	raw, _ := json.Marshal(map[string]any{"query": "no-such-command", "offset": page["next_offset"], "version": page["version"]})
	next, _ := tool.Call(t.Context(), raw, nil)
	if next.IsError || next.Flatten() == r.Flatten() {
		t.Fatal("pagination failed")
	}
	// New role bindings may become visible only after a new runner is assembled.
	rows = append(rows, &db.Tool{Key: "late-added", Kind: "shell", Enabled: true, Agents: []string{"worker"}})
	r, _ = tool.Call(t.Context(), json.RawMessage(`{"name":"late-added"}`), nil)
	if !r.IsError {
		t.Fatal("expanded run authorization")
	}
}

func TestShellHelpNameCollisionPreservesExistingTool(t *testing.T) {
	rows := shellHelpFixture()
	existing := actool.Build(actool.Spec{Name: agent.ShellToolHelpName, Description: "existing"})
	tools := configureShellToolHelp([]actool.CoreTool{actool.NewBash(), existing}, rows, "worker", nil)
	if len(tools) != 2 || tools[1] != existing || !strings.Contains(tools[0].Description(), "TAIL_RULE") {
		t.Fatal("shadowed existing tool or silently dropped configuration")
	}
}
