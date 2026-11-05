package agent

import (
	"context"
	"iter"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/runtimebudget"
)

type compactionUsageProvider struct{ llm.Provider }

func (p compactionUsageProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.Provider.Complete(runtimebudget.WithModelPhase(ctx, "compaction"), req)
}

func (p compactionUsageProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return p.Provider.Stream(runtimebudget.WithModelPhase(ctx, "compaction"), req)
}

// Use the same public upstream compactor, with attribution on its dedicated
// summarizer provider. Custom/noa compactors remain untouched.
func configureCompactionUsage(opts *agentcore.Options) {
	if opts.Compactor != nil || opts.Compaction == nil || opts.Provider == nil {
		return
	}
	opts.Compactor = compaction.New(*opts.Compaction, compaction.ProviderSummarizer(compactionUsageProvider{opts.Provider}, opts.MaxTokens, opts.NonStreaming))
	opts.Compaction = nil
}
