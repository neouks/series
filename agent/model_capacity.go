package agent

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"iter"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/neouks/series/runtimebudget"
)

const rejectedModelRequests = 64
const rejectedModelTTL = 2 * time.Minute

type measuredModelInput struct {
	at     time.Time
	tokens int
}

// ValidateModelCapacity is shared by profile writes and direct construction.
// Zero retains legacy endpoint defaults; it is not an assertion of actual
// capacity. An output that occupies the whole declared window cannot fit input.
func (c Config) ValidateModelCapacity() error {
	if c.ContextWindowK < 0 || c.ContextWindowK > maxWindowK {
		return fmt.Errorf("模型上下文窗口须为0（未声明）或1..1000 K token")
	}
	if c.MaxTokens < 0 {
		return fmt.Errorf("模型输出上限不能为负数")
	}
	if c.ContextWindowK > 0 && c.MaxTokens >= c.CompactionWindow() {
		return &runtimebudget.InvalidOutputBudget{Window: c.CompactionWindow(), Output: c.MaxTokens}
	}
	return nil
}

type rejectedModelRequest struct {
	at     time.Time
	window int
}

// This boundary runs on the final request that this endpoint actually receives,
// after role authorization and request projection. Estimates are not treated as
// exact token counts. Impossible output budgets, identical inputs with endpoint
// accounting, and previously rejected requests can be stopped locally. SDK
// compaction remains free to send a different request; raw history and evidence
// are never rewritten.
type modelCapacityProvider struct {
	llm.Provider
	configured int
	format     llm.Format
	mu         sync.Mutex
	rejected   map[[32]byte]rejectedModelRequest
	measured   map[[32]byte]measuredModelInput
	reported   rejectedModelRequest
	now        func() time.Time
}

func newModelCapacityProvider(p llm.Provider, c Config) *modelCapacityProvider {
	window := 0
	if c.ContextWindowK > 0 {
		window = c.CompactionWindow()
	}
	return &modelCapacityProvider{Provider: p, configured: window, format: c.Format, now: time.Now}
}

func (p *modelCapacityProvider) before(ctx context.Context, req llm.CompletionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	byteLimit := int64(runtimebudget.ModelBytePolicy(ctx).RequestMiB) * runtimebudget.MiB
	if owner, _ := ctx.Value(modelDeadlineKey{}).(*modelCallDeadline); owner != nil {
		byteLimit = int64(owner.bytes.limits.RequestMiB) * runtimebudget.MiB
	}
	if err := runtimebudget.CheckModelInputBytes(req, byteLimit); err != nil {
		return err
	}
	p.mu.Lock()
	now := p.now()
	window := p.configured
	if p.reported.window > 0 && now.Sub(p.reported.at) < rejectedModelTTL {
		if window == 0 || p.reported.window < window {
			window = p.reported.window
		}
	}
	output := req.MaxTokens
	// Upstream Anthropic supplies 8192 when the caller leaves MaxTokens unset.
	// OpenAI leaves it to the endpoint: zero remains unknown, not zero reserve.
	if output == 0 && (p.format == "" || p.format == llm.FormatAnthropic) {
		output = 8192
	}
	if window > 0 && output >= window {
		p.mu.Unlock()
		return &runtimebudget.InvalidOutputBudget{Window: window, Output: output}
	}
	for key, rejected := range p.rejected {
		if now.Sub(rejected.at) >= rejectedModelTTL {
			delete(p.rejected, key)
		}
	}
	hasRejected := len(p.rejected) != 0
	for key, value := range p.measured {
		if now.Sub(value.at) >= rejectedModelTTL {
			delete(p.measured, key)
		}
	}
	hasMeasured := window > 0 && len(p.measured) != 0
	p.mu.Unlock()
	if !hasRejected && !hasMeasured {
		return nil
	}
	if hasMeasured {
		if key, ok := modelInputFingerprint(ctx, req); ok {
			p.mu.Lock()
			value, exists := p.measured[key]
			p.mu.Unlock()
			if exists && now.Sub(value.at) < rejectedModelTTL && (value.tokens >= window || output > window-value.tokens) {
				return &runtimebudget.MeasuredCapacityError{Window: window, Input: value.tokens, Output: output}
			}
		}
	}
	if !hasRejected {
		return nil
	}
	key, ok := modelRequestFingerprint(ctx, req)
	if !ok {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if rejected, exists := p.rejected[key]; exists {
		return &runtimebudget.CapacityError{ConfiguredWindow: p.configured, ReportedWindow: rejected.window, Repeated: true}
	}
	return nil
}

func (p *modelCapacityProvider) after(ctx context.Context, req llm.CompletionRequest, err error) error {
	if !runtimebudget.IsContextCapacityRejection(err) || ctx.Err() != nil {
		return err
	}
	window := runtimebudget.RejectedContextWindow(err)
	key, ok := modelRequestFingerprint(ctx, req)
	if ok {
		p.mu.Lock()
		// A reported endpoint window is shared only within this immutable
		// provider/configuration, expires, and never enlarges the configured one.
		if window > 0 {
			learned := window
			if p.reported.window > 0 && p.now().Sub(p.reported.at) < rejectedModelTTL {
				learned = min(window, p.reported.window)
			}
			p.reported = rejectedModelRequest{at: p.now(), window: learned}
		}
		if p.rejected == nil {
			p.rejected = make(map[[32]byte]rejectedModelRequest)
		}
		if len(p.rejected) >= rejectedModelRequests {
			var oldest [32]byte
			at := p.now()
			for k, item := range p.rejected {
				if !item.at.After(at) {
					oldest, at = k, item.at
				}
			}
			delete(p.rejected, oldest)
		}
		p.rejected[key] = rejectedModelRequest{at: p.now(), window: window}
		p.mu.Unlock()
	}
	ri := RunInfoFrom(ctx)
	log.Printf("[model-capacity-rejection] task=%d role=%s configured_tokens=%d reported_tokens=%d", ri.TaskID, ri.AgentKey, p.configured, window)
	return &runtimebudget.CapacityError{ConfiguredWindow: p.configured, ReportedWindow: window}
}

// Cache only numeric accounting plus a run-scoped hash. Canonical SDK usage
// already includes cached input; adding CacheRead/Write again overcounts it.
func (p *modelCapacityProvider) observe(ctx context.Context, req llm.CompletionRequest, usage llm.Usage) {
	if usage.InputTokens <= 0 || ctx.Err() != nil {
		return
	}
	key, ok := modelInputFingerprint(ctx, req)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.measured == nil {
		p.measured = make(map[[32]byte]measuredModelInput)
	}
	if len(p.measured) >= rejectedModelRequests {
		var oldest [32]byte
		at := p.now()
		for k, value := range p.measured {
			if !value.at.After(at) {
				oldest, at = k, value.at
			}
		}
		delete(p.measured, oldest)
	}
	p.measured[key] = measuredModelInput{at: p.now(), tokens: usage.InputTokens}
}

func modelInputFingerprint(ctx context.Context, req llm.CompletionRequest) ([32]byte, bool) {
	// Output capacity does not change the input in the three pinned adapters.
	// Retain every other option, tool schema, signature and message boundary.
	req.MaxTokens = 0
	return modelRequestFingerprint(ctx, req)
}

// JSON fields are encoded independently: hashing a rejected long history must
// not allocate a second full-history JSON buffer. The cache stores only hashes,
// times and numeric limits and is scoped to the trusted run plus this provider.
func modelRequestFingerprint(ctx context.Context, req llm.CompletionRequest) ([32]byte, bool) {
	h := sha256.New()
	encode := func(value any) bool { return json.NewEncoder(h).Encode(value) == nil }
	// json.Encoder compacts RawMessage, whereas the OpenAI adapter replays
	// tool arguments as their original string (including whitespace). Preserve
	// those exact bytes as well: semantically equal JSON is not equal token input.
	var rawInputs func([]llm.ContentBlock)
	rawInputs = func(blocks []llm.ContentBlock) {
		for _, block := range blocks {
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(block.Input)))
			h.Write(size[:])
			h.Write(block.Input)
			rawInputs(block.Content)
		}
	}
	ri := RunInfoFrom(ctx)
	if !encode([]any{ri.TaskID, ri.ExplorationID, ri.IntentID, ri.AgentKey, transcript.SessionIDFrom(ctx)}) {
		return [32]byte{}, false
	}
	base := req
	base.System, base.Tools, base.Messages = nil, nil, nil
	if !encode(base) {
		return [32]byte{}, false
	}
	for _, values := range []struct {
		label string
		count int
	}{{"system", len(req.System)}, {"tools", len(req.Tools)}, {"messages", len(req.Messages)}} {
		if !encode(values.label) || !encode(values.count) {
			return [32]byte{}, false
		}
		switch values.label {
		case "system":
			for _, v := range req.System {
				if !encode(v) {
					return [32]byte{}, false
				}
			}
		case "tools":
			for _, v := range req.Tools {
				if !encode(v) {
					return [32]byte{}, false
				}
			}
		case "messages":
			for _, v := range req.Messages {
				if !encode(v) {
					return [32]byte{}, false
				}
				rawInputs(v.Content)
			}
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, true
}

func (p *modelCapacityProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if err := p.before(ctx, req); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	msg, stop, usage, err := p.Provider.Complete(ctx, req)
	if err == nil {
		p.observe(ctx, req, usage)
	}
	return msg, stop, usage, p.after(ctx, req, err)
}

func (p *modelCapacityProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if err := p.before(ctx, req); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		var usage llm.Usage
		terminal, failed := false, false
		defer func() {
			if terminal && !failed {
				p.observe(ctx, req, usage)
			}
		}()
		for event, err := range p.Provider.Stream(ctx, req) {
			failed = failed || err != nil
			usage.InputTokens = max(usage.InputTokens, event.Usage.InputTokens)
			terminal = terminal || event.Type == llm.SEMessageStop || (event.Type == llm.SEMessageDelta && event.StopReason != "")
			if !yield(event, p.after(ctx, req, err)) || err != nil {
				return
			}
		}
	}
}
