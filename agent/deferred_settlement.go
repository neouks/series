package agent

import (
	"slices"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// The SDK signals settlement by removing configured schemas. Deferred targets
// already have no schema, so retain an internal phase marker for the dispatch
// hook. Strip it before sending the request to the model; never infer phase from
// user-controlled prompt text. The marker is not an executable capability.
const settlementMarkerName = "__artex_execution_phase"

func configureDeferredSettlement(opts *agentcore.Options) {
	if opts.Settlement == nil {
		return
	}
	needed := false
	for _, tool := range opts.Tools {
		needed = needed || builtinPolicy(tool) != nil
	}
	for _, name := range opts.Settlement.DisabledTools {
		needed = needed || slices.Contains(opts.DeferredTools, name)
	}
	if !needed {
		return
	}
	settlement := *opts.Settlement
	settlement.DisabledTools = append(slices.Clone(settlement.DisabledTools), settlementMarkerName)
	opts.Settlement = &settlement
	opts.Tools = append(slices.Clone(opts.Tools), actool.Build(actool.Spec{Name: settlementMarkerName, Schema: map[string]any{"type": "object"}}))
}

func withoutSettlementMarker(req llm.CompletionRequest) llm.CompletionRequest {
	for i, tool := range req.Tools {
		if tool.Name == settlementMarkerName {
			req.Tools = append(slices.Clone(req.Tools[:i]), req.Tools[i+1:]...)
			break
		}
	}
	return req
}
