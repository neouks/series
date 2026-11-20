//go:build liveeval

package agent

// Opt-in policy ablation: real SDK/provider and real request projection, with
// isolated handlers supplied by the caller. No production model/tool settings
// are changed. The pre-native arm deliberately skips ConfigureBuiltinTools.
import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func RunLiveBuiltinEvaluation(ctx context.Context, opts agentcore.Options, prompt, variant string, observe func(LiveToolRequest)) (LiveToolRun, error) {
	result := LiveToolRun{Variant: variant}
	if variant != "stable" && variant != "before" {
		return result, fmt.Errorf("old idle/whole-request-pressure controls must use their frozen 20261120 binaries")
	}
	if opts.Provider == nil {
		return result, fmt.Errorf("real provider is required")
	}
	if variant != "before" {
		opts.Tools = ConfigureBuiltinTools(opts.Tools, BuiltinLoadingOptions{Role: "mainagent", SearchAllowed: true})
	}
	var def DeferredInfo
	opts.Tools, def = OnDemandTools(opts.Tools, def, "mainagent")
	opts.DeferredTools, opts.UnlockSet = def.Deferred, def.Unlock
	opts.SystemPrompt = append(append([]string(nil), opts.SystemPrompt...), toolDocumentationWindowRule)
	if block := actool.RenderDeferredToolsBlock(def.GlobalNames); block != "" {
		opts.SystemPrompt = append(opts.SystemPrompt, block)
	}
	opts.DynamicBoundary = len(opts.SystemPrompt)
	policy := newDispatchHooks(opts)
	opts.Tools = append([]actool.CoreTool(nil), opts.Tools...)
	for i, tool := range opts.Tools {
		opts.Tools[i] = &processContextTool{CoreTool: tool, hooks: policy}
		policy.tools[tool.Name()] = opts.Tools[i]
	}
	opts.Hooks, opts.NonStreaming, opts.DisableBackgroundTasks = policy, true, true
	// Preserve the configured context limit in policy.schemaPressure; disable
	// summaries/automatic retries equally in all arms for attributable accounting.
	opts.Compaction, opts.Compactor = nil, nil
	provider := opts.Provider
	opts.Deps.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		encoded, _ := json.Marshal(req)
		start := time.Now()
		message, stop, usage, err := provider.Complete(ctx, req)
		entry := LiveToolRequest{Turn: len(result.Requests) + 1, Milliseconds: time.Since(start).Milliseconds(), RequestBytes: len(encoded), Usage: usage, Request: req, Response: message}
		entry.InputEstimate = requestInputEstimate(req)
		available := map[string]bool{}
		for _, schema := range req.Tools {
			available[schema.Name] = true
		}
		for _, found := range discoveryHistory(req) {
			if contracts, ok := retainedContracts(found.text); ok {
				for _, contract := range contracts {
					available[contract.Name] = true
				}
			}
		}
		for name := range available {
			entry.AvailableSchemas = append(entry.AvailableSchemas, name)
		}
		sort.Strings(entry.AvailableSchemas)
		if err != nil {
			entry.Error = err.Error()
		}
		result.Requests = append(result.Requests, entry)
		result.Usage.Add(usage)
		if observe != nil {
			observe(entry)
		}
		return message, stop, usage, err
	}
	policy.observeModelCalls(&opts)
	session := agentcore.NewSession(opts)
	defer session.Close()
	for event := range session.Prompt(ctx, prompt) {
		if event.Kind == harness.KindResult && event.Terminal != nil {
			result.Final, result.Reason = event.Terminal.Text, event.Terminal.Reason
			if event.Terminal.Err != nil {
				result.Error = event.Terminal.Err.Error()
			}
		}
	}
	result.Messages = session.Messages()
	return result, nil
}
