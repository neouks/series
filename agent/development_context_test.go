package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// These comparisons use synthetic development data. The representations here
// are scoped to the test binary.
func TestDevelopmentContextRepresentations(t *testing.T) {
	t.Run("execution output", func(t *testing.T) {
		lines := make([]string, 240)
		for i := range lines {
			lines[i] = fmt.Sprintf("package build/fixture%03d passed compiler and unit checks", i)
		}
		lines[117] = "error: fixture117 did not generate version.txt"
		log := strings.Join(lines, "\n") + "\n"

		view, stored := fixtureBuildView(log, 1, 4)
		if view.ExitCode != 1 || view.TotalLines != len(lines) || !strings.Contains(view.Preview, lines[117]) {
			t.Fatalf("initial view lost build status or diagnostics: %+v", view)
		}
		original, ok := stored[view.FullOutput]
		if !ok || strings.Join(original, "") != log {
			t.Fatal("full-output reference did not resolve to the original log")
		}
		reportDevelopmentContextBytes(t, log, view)
		diagnosticWindow := strings.Join(original[116:119], "")
		if diagnosticWindow != strings.Join(lines[116:119], "\n")+"\n" {
			t.Fatal("could not recover the diagnostic with neighboring lines")
		}

		short := "build passed\n"
		view, _ = fixtureBuildView(short, 0, 4)
		if view.Preview != short || view.FullOutput != "" {
			t.Fatalf("short output should stay inline: %+v", view)
		}
	})

	t.Run("snapshot fields", func(t *testing.T) {
		packages := make([]string, 150)
		for i := range packages {
			packages[i] = fmt.Sprintf("build/fixture/package%03d", i)
		}
		var snapshots []map[string]any
		for i := 0; i < 8; i++ {
			snapshots = append(snapshots, map[string]any{
				"packages": packages, "status": "running", "completed": i,
			})
		}
		snapshots[len(snapshots)-1]["status"] = "passed"
		snapshots[2]["warning"] = "optional report skipped"

		view := fixtureSnapshotView(snapshots)
		if len(view.Changes[1].Changed) != 1 || !reflect.DeepEqual(view.Changes[3].Removed, []string{"warning"}) {
			t.Fatalf("updates and deletions were not captured: %+v", view.Changes)
		}
		current := cloneFixtureSnapshot(view.Latest)
		for i := len(view.Changes) - 1; i >= 0; i-- {
			if !reflect.DeepEqual(current, snapshots[i]) {
				t.Fatalf("snapshot %d did not round-trip", i)
			}
			for _, field := range view.Changes[i].Added {
				delete(current, field)
			}
			for _, field := range view.Changes[i].Removed {
				current[field] = view.Changes[i].Previous[field]
			}
			for _, field := range view.Changes[i].Changed {
				current[field] = view.Changes[i].Previous[field]
			}
		}
		reportDevelopmentContextBytes(t, snapshots, view)
	})

	t.Run("tool declarations", func(t *testing.T) {
		definitions := map[string]fixtureTool{
			"read_build_status":  newFixtureTool("Read build status."),
			"read_test_summary":  newFixtureTool("Read the test summary."),
			"read_package_stats": newFixtureTool("Read package statistics."),
		}
		catalog := make(map[string]string, len(definitions))
		for name, definition := range definitions {
			catalog[name] = definition.Purpose
		}
		help, found := definitions["read_test_summary"]
		if !found || help.Parameters.Properties["line"].Type != "integer" {
			t.Fatal("selected tool must retain its complete parameter definition")
		}
		view := struct {
			Catalog map[string]string      `json:"catalog"`
			Loaded  map[string]fixtureTool `json:"loaded"`
		}{catalog, map[string]fixtureTool{"read_test_summary": help}}
		reportDevelopmentContextBytes(t, definitions, view)
	})
}

func TestDevelopmentContextAgentSession(t *testing.T) {
	const (
		overviewName = "read_fixture_build"
		lineName     = "read_fixture_log_line"
		summaryName  = "read_fixture_test_summary"
		middleLine   = "generated report id: fixture-45"
	)
	log := strings.Repeat("compiler check passed\n", 45) + middleLine + "\n" +
		strings.Repeat("unit check passed\n", 45)
	output, stored := fixtureBuildView(log, 0, 4)
	packages := strings.Repeat("fixture package list; ", 80)
	snapshots := fixtureSnapshotView([]map[string]any{
		{"packages": packages, "status": "running"},
		{"packages": packages, "status": "passed"},
	})
	overview, err := json.Marshal(struct {
		Output    fixtureOutput    `json:"output"`
		Snapshots fixtureSnapshots `json:"snapshots"`
	}{output, snapshots})
	if err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ref":  map[string]any{"type": "string"},
			"line": map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []string{"ref", "line"},
	}

	for _, stream := range []bool{false, true} {
		for _, hosted := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/hosted=%t", stream, hosted), func(t *testing.T) {
				lineCalls, unusedCalls := 0, 0
				overviewTool := actool.Build(actool.Spec{
					Name: overviewName, Description: "Read the fixed build overview and snapshot history.",
					Schema: obj(nil),
					Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
						return actool.Text(string(overview)), nil
					},
				})
				lineTool := actool.Build(actool.Spec{
					Name: lineName, Description: "Read one line from the synthetic build log.", Schema: schema,
					Run: func(_ context.Context, raw json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
						lineCalls++
						var in struct {
							Ref  string `json:"ref"`
							Line int    `json:"line"`
						}
						if json.Unmarshal(raw, &in) != nil || in.Line < 1 || in.Line > len(stored[in.Ref]) {
							return actool.Errorf("unknown fixture reference or line"), nil
						}
						return actool.Text(stored[in.Ref][in.Line-1]), nil
					},
				})
				summaryTool := actool.Build(actool.Spec{
					Name: summaryName, Description: "Read a separate synthetic test summary.", Schema: obj(nil),
					Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
						unusedCalls++
						return actool.Text("fixture passed"), nil
					},
				})
				tools := ConfigureBuiltinTools([]actool.CoreTool{overviewTool, lineTool, summaryTool}, BuiltinLoadingOptions{
					Role: "development_fixture", SearchAllowed: true,
					Known: map[string]bool{lineName: true, summaryName: true, BuiltinSearchName: true},
				})

				turn := 0
				complete := func(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					current := turn
					turn++
					if !hasBuiltin(req, overviewName) || !hasBuiltin(req, BuiltinSearchName) ||
						hasBuiltin(req, summaryName) || hasBuiltin(req, lineName) != (current >= 2) {
						t.Errorf("turn %d: unexpected fixture tool visibility", current)
					}
					last := developmentFixtureResult(req)
					var next llm.Message
					switch current {
					case 0:
						next = documentationCall("overview", overviewName, `{}`)
					case 1:
						if last.IsError || len(last.Content) != 1 || last.Content[0].Text != string(overview) ||
							strings.Contains(last.Content[0].Text, middleLine) ||
							strings.Count(last.Content[0].Text, packages) != 1 {
							t.Error("overview lost its reference or duplicated the snapshot payload")
						}
						next = documentationCall("search", BuiltinSearchName, `{"mode":"exact","query":"read_fixture_log_line"}`)
					case 2:
						for _, declaration := range req.Tools {
							if declaration.Name == lineName && !reflect.DeepEqual(declaration.InputSchema, schema) {
								t.Error("loading changed the parameter schema")
							}
						}
						next = documentationCall("invalid", lineName, `{"ref":"missing","line":46}`)
					case 3:
						if !last.IsError {
							t.Error("unknown log reference was accepted")
						}
						next = documentationCall("line", lineName, `{"ref":"build-log","line":46}`)
					case 4:
						if last.IsError || len(last.Content) != 1 || last.Content[0].Text != middleLine+"\n" {
							t.Error("middle log line was not recovered exactly")
						}
						return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("checked")}}, "end_turn", llm.Usage{}, nil
					default:
						return llm.Message{}, "", llm.Usage{}, fmt.Errorf("unexpected fixture model turn %d", current)
					}
					return next, "tool_use", llm.Usage{}, nil
				}
				opts := agentcore.Options{
					Tools: tools, NonStreaming: !stream, DisableBackgroundTasks: true,
					PermissionMode: permission.ModeBypass, Deps: sdkTurnDeps(complete),
				}
				if hosted {
					opts.Transcript = transcript.NewStore(t.TempDir())
				}
				answer, _, err := captureRun(t.Context(), opts, "Check the synthetic build history and recover line 46.", nil)
				if err != nil || answer != "checked" || turn != 5 || lineCalls != 2 || unusedCalls != 0 {
					t.Fatalf("answer=%q turns=%d line calls=%d unrelated calls=%d error=%v", answer, turn, lineCalls, unusedCalls, err)
				}
			})
		}
	}
}

func developmentFixtureResult(req llm.CompletionRequest) llm.ContentBlock {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		for _, block := range req.Messages[i].Content {
			if block.Type == llm.BlockToolResult && len(block.Content) > 0 {
				return block
			}
		}
	}
	return llm.ContentBlock{}
}

type fixtureOutput struct {
	ExitCode   int    `json:"exit_code"`
	TotalLines int    `json:"total_lines"`
	Preview    string `json:"preview"`
	FullOutput string `json:"full_output,omitempty"`
}

func fixtureBuildView(log string, exitCode, edgeLines int) (fixtureOutput, map[string][]string) {
	lines := strings.SplitAfter(log, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	view := fixtureOutput{ExitCode: exitCode, TotalLines: len(lines), Preview: log}
	if len(lines) <= 2*edgeLines {
		return view, nil
	}
	var preview strings.Builder
	for i, line := range lines {
		if i < edgeLines || i >= len(lines)-edgeLines || strings.HasPrefix(line, "error:") {
			fmt.Fprintf(&preview, "%d: %s", i+1, line)
		}
	}
	view.Preview = preview.String()
	view.FullOutput = "build-log"
	return view, map[string][]string{view.FullOutput: lines}
}

type fixtureSnapshotChange struct {
	Added    []string       `json:"added,omitempty"`
	Removed  []string       `json:"removed,omitempty"`
	Changed  []string       `json:"changed,omitempty"`
	Previous map[string]any `json:"previous,omitempty"`
}

type fixtureSnapshots struct {
	Latest  map[string]any          `json:"latest"`
	Changes []fixtureSnapshotChange `json:"changes"`
}

func fixtureSnapshotView(snapshots []map[string]any) fixtureSnapshots {
	view := fixtureSnapshots{
		Latest: snapshots[len(snapshots)-1], Changes: make([]fixtureSnapshotChange, len(snapshots)),
	}
	for i := 1; i < len(snapshots); i++ {
		before, after := snapshots[i-1], snapshots[i]
		change := fixtureSnapshotChange{Previous: map[string]any{}}
		for field, value := range before {
			switch next, present := after[field]; {
			case !present:
				change.Removed = append(change.Removed, field)
				change.Previous[field] = value
			case !reflect.DeepEqual(value, next):
				change.Changed = append(change.Changed, field)
				change.Previous[field] = value
			}
		}
		for field := range after {
			if _, present := before[field]; !present {
				change.Added = append(change.Added, field)
			}
		}
		sort.Strings(change.Added)
		sort.Strings(change.Removed)
		sort.Strings(change.Changed)
		view.Changes[i] = change
	}
	return view
}

func cloneFixtureSnapshot(snapshot map[string]any) map[string]any {
	out := make(map[string]any, len(snapshot))
	for field, value := range snapshot {
		out[field] = value
	}
	return out
}

type fixtureParameter struct {
	Type string `json:"type"`
}

type fixtureTool struct {
	Purpose     string `json:"purpose"`
	Description string `json:"description"`
	Parameters  struct {
		Type       string                      `json:"type"`
		Properties map[string]fixtureParameter `json:"properties"`
		Required   []string                    `json:"required"`
	} `json:"parameters"`
}

func newFixtureTool(purpose string) fixtureTool {
	tool := fixtureTool{
		Purpose:     purpose,
		Description: purpose + " The line number is one-based and refers to the immutable build report.",
	}
	tool.Parameters.Type = "object"
	tool.Parameters.Properties = map[string]fixtureParameter{"line": {Type: "integer"}}
	tool.Parameters.Required = []string{"line"}
	return tool
}

func reportDevelopmentContextBytes(t *testing.T, original, projected any) {
	t.Helper()
	before, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) >= len(before) {
		t.Fatalf("projection grew: %d to %d bytes", len(before), len(after))
	}
	t.Logf("serialized JSON: %d → %d bytes", len(before), len(after))
}
