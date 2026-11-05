package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// The upstream compactor reserves a fixed 13K buffer plus 15K predicted tool
// growth. Raising the advertised model capacity hides an actual small window;
// applying that fixed trigger directly repeatedly summarizes healthy sessions.
// Use the public Compactor interface to scale the trigger only. Summary format,
// boundaries, preserved tail and raw transcript remain upstream-owned.
type smallWindowCompactor struct {
	inner            harness.Compactor
	threshold, fixed int
	keepRecent       int
	lastHead         [32]byte
	lastAttempt      time.Time
}

func configureSmallWindowCompaction(opts *agentcore.Options) {
	if opts.Compactor != nil || opts.Compaction == nil || opts.Provider == nil {
		return // Preserve noa and explicitly supplied context managers.
	}
	cfg := *opts.Compaction
	if opts.MaxTokens > 0 {
		cfg.MaxOutputTokens = opts.MaxTokens
	}
	opts.Compaction = &cfg
	if cfg.ContextWindow <= 0 || cfg.ContextWindow >= 32000 {
		return
	}
	output := min(8000, max(1, cfg.ContextWindow/4))
	if opts.MaxTokens > 0 {
		output = min(output, opts.MaxTokens)
	}
	cfg.MaxOutputTokens = output
	fixedBytes := 0
	for _, text := range append(append([]string{}, opts.SystemPrompt...), opts.AppendSystemPrompt...) {
		fixedBytes += len(text)
	}
	deferred := make(map[string]bool, len(opts.DeferredTools))
	for _, name := range opts.DeferredTools {
		deferred[name] = true
	}
	for _, tool := range opts.Tools {
		if deferred[tool.Name()] {
			continue
		}
		raw, err := json.Marshal(llm.ToolSchema{Name: tool.Name(), Description: tool.Description(), InputSchema: tool.InputSchema()})
		if err == nil {
			fixedBytes += len(raw)
		}
	}
	// This is an estimate to trigger compression, not a physical token limit.
	fixed := (fixedBytes + 2) / 3
	available := cfg.ContextWindow - output - fixed - max(1, cfg.ContextWindow/10)
	opts.Compactor = &smallWindowCompactor{
		inner:     compaction.New(cfg, compaction.ProviderSummarizer(compactionUsageProvider{opts.Provider}, output, opts.NonStreaming)),
		threshold: available * 8 / 10, fixed: fixed,
		keepRecent: max(1, cfg.KeepRecent),
	}
	if cfg.KeepRecent == 0 {
		opts.Compactor.(*smallWindowCompactor).keepRecent = 8
	}
	opts.Compaction = nil
}

func (c *smallWindowCompactor) Pre(ctx context.Context, msgs []llm.Message, lastInputTokens int) []llm.Message {
	if c.threshold <= 0 {
		return msgs // Shrinking history cannot fix oversized fixed instructions.
	}
	estimate := compaction.EstimateTokens(llm.MessagesForAPI(msgs)) * 4 / 3
	if max(estimate, lastInputTokens-c.fixed) < c.threshold {
		return msgs
	}
	start := llm.LastBoundaryIndex(msgs) + 1
	tail, count := len(msgs), 0
	for tail > start && count < c.keepRecent {
		tail--
		if !llm.IsBoundaryMarker(msgs[tail]) {
			count++
		}
	}
	if tail <= start || (start > 0 && tail-start == 1) {
		return msgs // No eligible head, or only the previous summary remains.
	}
	key, ok := modelRequestFingerprint(ctx, llm.CompletionRequest{Messages: msgs[start:tail]})
	if ok {
		if key == c.lastHead && time.Since(c.lastAttempt) < time.Minute {
			return msgs
		}
		c.lastHead, c.lastAttempt = key, time.Now()
	}
	out, _ := c.inner.Reactive(ctx, msgs)
	return out
}

func (c *smallWindowCompactor) Reactive(ctx context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	return c.inner.Reactive(ctx, msgs)
}

func (c *smallWindowCompactor) IsOverflow(err error) bool { return c.inner.IsOverflow(err) }
