//go:build liveeval

package agent

import (
	"context"
	"encoding/json"
	"iter"
	"testing"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

type builtinEvaluationProvider struct {
	complete func(llm.CompletionRequest) (llm.Message, string, llm.Usage, error)
}

func (p builtinEvaluationProvider) Complete(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.complete(r)
}
func (p builtinEvaluationProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	panic("non-streaming test")
}

func TestLiveBuiltinRunnerUsesProductionProjection(t *testing.T) {
	for _, variant := range []string{"before", "stable"} {
		t.Run(variant, func(t *testing.T) {
			turn := 0
			tool := actool.Build(actool.Spec{Name: "asset_get", Schema: obj(nil), Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				return actool.Text("ok"), nil
			}})
			provider := builtinEvaluationProvider{complete: func(req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				active := hasBuiltin(req, "asset_get")
				want := true
				if active != want {
					t.Fatalf("turn %d active=%v want=%v", turn, active, want)
				}
				turn++
				if variant == "before" || turn == 9 {
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}, "end_turn", llm.Usage{}, nil
				}
				query := `{"mode":"catalog","query":""}`
				if turn == 1 {
					query = `{"mode":"exact","query":"asset_get"}`
				}
				return documentationCall(string(rune('a'+turn)), BuiltinSearchName, query), "tool_use", llm.Usage{}, nil
			}}
			run, err := RunLiveBuiltinEvaluation(t.Context(), agentcore.Options{Provider: provider, Tools: []actool.CoreTool{tool}, PermissionMode: permission.ModeBypass}, "go", variant, nil)
			if err != nil || run.Error != "" {
				t.Fatal(err, run.Error)
			}
		})
	}
}
