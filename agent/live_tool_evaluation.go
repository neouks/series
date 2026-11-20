//go:build liveeval

package agent

// This opt-in harness is excluded from production builds. All arms use the same
// SDK loop, tool registry, parameter preparation, permissions and transport.
// Only documentation projection and its associated schema gate differ.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type LiveToolRequest struct {
	Turn             int                   `json:"turn"`
	Milliseconds     int64                 `json:"milliseconds"`
	RequestBytes     int                   `json:"request_bytes"`
	InputEstimate    int                   `json:"input_estimate,omitempty"`
	AvailableSchemas []string              `json:"available_schemas,omitempty"`
	Usage            llm.Usage             `json:"usage"`
	Request          llm.CompletionRequest `json:"request"`
	Response         llm.Message           `json:"response"`
	Error            string                `json:"error,omitempty"`
}

type LiveToolRun struct {
	Variant  string                 `json:"variant"`
	Final    string                 `json:"final"`
	Reason   harness.TerminalReason `json:"reason"`
	Error    string                 `json:"error,omitempty"`
	Usage    llm.Usage              `json:"usage"`
	Requests []LiveToolRequest      `json:"requests"`
	Messages []llm.Message          `json:"messages"`
}

func RunLiveToolEvaluation(ctx context.Context, opts agentcore.Options, prompt, variant string, observe func(LiveToolRequest)) (LiveToolRun, error) {
	result := LiveToolRun{Variant: variant}
	if variant != "full_history" && variant != "doc_window" && variant != "schema_window" && variant != "pressure_window" {
		return result, fmt.Errorf("unknown evaluation variant %q", variant)
	}
	if opts.Provider == nil {
		return result, fmt.Errorf("real provider is required")
	}
	policy := newDispatchHooks(opts)
	if variant != "schema_window" && variant != "pressure_window" {
		policy.schemaRequired = map[string]bool{}
	}
	if variant == "schema_window" {
		opts.SystemPrompt = append(append([]string(nil), opts.SystemPrompt...), legacyToolDocumentationWindowRule)
	}
	if variant == "pressure_window" {
		opts.SystemPrompt = append(append([]string(nil), opts.SystemPrompt...), toolDocumentationWindowRule)
	}
	opts.Tools = append([]actool.CoreTool(nil), opts.Tools...)
	for i, tool := range opts.Tools {
		opts.Tools[i] = &processContextTool{CoreTool: tool, hooks: policy}
		policy.tools[tool.Name()] = opts.Tools[i]
	}
	opts.Hooks = policy
	opts.NonStreaming = true
	opts.DisableBackgroundTasks = true
	opts.Compaction, opts.Compactor = nil, nil
	provider := opts.Provider
	opts.Deps.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		policy.observeTools(req.Tools)
		switch variant {
		case "schema_window":
			req = legacyProjectToolDocumentation(req)
		case "pressure_window":
			req = policy.projectToolDocumentation(req)
		default:
			// Common pre-existing identical-query deduplication in both controls.
			req = governRequestDocs(deduplicateReadHistoryBy(req, 1024, 1024, func(name string, raw json.RawMessage) string {
				if name != actool.SearchExtraToolsName {
					return ""
				}
				var args struct {
					Query string `json:"query"`
				}
				if json.Unmarshal(raw, &args) != nil || args.Query == "" {
					return ""
				}
				return name + ":" + args.Query
			}))
			if variant == "doc_window" {
				req = legacyWindowToolDocumentation(req)
			}
		}
		if variant == "pressure_window" {
			policy.observeDiscoveredSchemas(req)
		} else {
			policy.legacyObserveDiscoveredSchemas(req)
		}
		encoded, _ := json.Marshal(req)
		start := time.Now()
		message, stop, usage, err := provider.Complete(ctx, req)
		entry := LiveToolRequest{Turn: len(result.Requests) + 1, Milliseconds: time.Since(start).Milliseconds(), RequestBytes: len(encoded), Usage: usage, Request: req, Response: message}
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
