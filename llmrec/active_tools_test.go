package llmrec

import (
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestActiveToolsReflectActualRequest(t *testing.T) {
	r := &Recorder{}
	for _, tools := range [][]llm.ToolSchema{nil, {{Name: "context_get"}, {Name: "report_finding"}}} {
		var recorded struct {
			Active []string `json:"active_tools"`
		}
		if err := json.Unmarshal([]byte(r.serializeRequest(llm.CompletionRequest{Tools: tools})), &recorded); err != nil {
			t.Fatal(err)
		}
		if recorded.Active == nil || len(recorded.Active) != len(tools) {
			t.Fatal("missing actual tools")
		}
		for i, tool := range tools {
			if recorded.Active[i] != tool.Name {
				t.Fatal("wrong tool recorded")
			}
		}
	}
}
