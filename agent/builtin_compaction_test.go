package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
)

func TestBuiltinLoadedDefinitionsSurviveActualCompaction(t *testing.T) {
	p, req := builtinFixture(1, nil)
	p.window = 24000
	req.MaxTokens = 4096
	p.project(t.Context(), req, false)
	builtinDiscover(t, p, builtinDiscoveryInput{Mode: "exact", Query: "probe_00"})
	loaded, _ := p.project(t.Context(), req, false)
	loaded = p.applyPressure(loaded)
	summaries := 0
	c := compaction.New(compaction.Config{ContextWindow: 32000}, func(context.Context, []llm.Message) (string, error) {
		summaries++
		return "保留任务状态，继续读取当前证据。", nil
	})
	req.Messages = retrievalFixture(100).Messages
	req.Messages = c.Pre(t.Context(), req.Messages, 300000)
	if summaries != 1 {
		t.Fatalf("no actual compaction: %d", summaries)
	}
	continued, _ := p.project(t.Context(), req, false)
	continued = p.applyPressure(continued)
	if !reflect.DeepEqual(loaded.Tools, continued.Tools) || !hasBuiltin(continued, "probe_00") {
		t.Fatal("summary discarded a loaded contract")
	}
}
