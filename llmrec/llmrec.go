// Package llmrec implements a recording decorator for llm.Provider. It intercepts
// every Stream call, captures the full request and accumulated response, and
// persists them to PostgreSQL for later inspection.
package llmrec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"iter"
	"log"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/neouks/series/db"
	"github.com/neouks/series/runtimebudget"
)

type taskIDContextKey struct{}
type runInfoContextKey struct{}
type inputRejectedMetricsKey struct{}

// RunInfo carries request attribution that cannot be recovered reliably from a
// transcript session id alone. It lives in llmrec (rather than agent) so the
// recorder can read it without an import cycle.
type RunInfo struct {
	TaskID        int64
	ExplorationID int64
	IntentID      int64
	SessionID     string
	AgentKey      string
	Trigger       string
	RetryOrdinal  int
	Phase         string
}

// WithRunInfo merges non-zero attribution into the current context. Merging lets
// the engine attach trigger/retry data before the agent adds task/run identity.
func WithRunInfo(ctx context.Context, next RunInfo) context.Context {
	current := RunInfoFrom(ctx)
	if next.TaskID > 0 {
		current.TaskID = next.TaskID
	}
	if next.ExplorationID > 0 {
		current.ExplorationID = next.ExplorationID
	}
	if next.IntentID > 0 {
		current.IntentID = next.IntentID
	}
	if next.SessionID != "" {
		current.SessionID = next.SessionID
	}
	if next.AgentKey != "" {
		current.AgentKey = next.AgentKey
	}
	if next.Trigger != "" {
		current.Trigger = next.Trigger
	}
	if next.RetryOrdinal > 0 {
		current.RetryOrdinal = next.RetryOrdinal
	}
	if next.Phase != "" {
		current.Phase = next.Phase
	}
	return context.WithValue(ctx, runInfoContextKey{}, current)
}

func RunInfoFrom(ctx context.Context) RunInfo {
	if ctx == nil {
		return RunInfo{}
	}
	info, _ := ctx.Value(runInfoContextKey{}).(RunInfo)
	return info
}

// WithIndependentRunInfo starts distinct attribution while retaining unrelated
// context values. Unlike WithRunInfo it cannot inherit an intent or retry from
// the caller that launched a detached run.
func WithIndependentRunInfo(ctx context.Context, info RunInfo) context.Context {
	return context.WithValue(ctx, runInfoContextKey{}, info)
}

// WithTaskID attaches the owning task registry id to an LLM call. Session ids
// are based on exploration ids, which are not interchangeable with task ids.
func WithTaskID(ctx context.Context, taskID string) context.Context {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ctx
	}
	return context.WithValue(ctx, taskIDContextKey{}, taskID)
}

// TaskIDFrom returns the explicit task registry id attached by the task runtime.
func TaskIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	taskID, _ := ctx.Value(taskIDContextKey{}).(string)
	return strings.TrimSpace(taskID)
}

// Recorder wraps an llm.Provider and records every completion call.
type Recorder struct {
	inner llm.Provider
	pg    *db.DB
	model string // model name (from config, not in CompletionRequest)
	prof  string // LLM profile name (from llm_profiles)
	// thinkingType / reasoningEffort 是配置级思考参数(思考开关 / 思考强度)。它们在
	// norma 的 buildBody() 里从 provider 配置注入真正的 HTTP body,不出现在
	// CompletionRequest 上,故 Recorder 需在此单独带一份,序列化时写进录制。
	thinkingType    string
	reasoningEffort string
	enabled         func() bool // reports whether recording is currently on; nil = always record
	writeTimeout    time.Duration
}

// Wrap returns a Provider that records calls to pg when enabled() reports true.
// profName is the LLM profile name (e.g. "default"); may be empty. thinkingType /
// reasoningEffort are the config-level thinking params actually sent to the API
// (empty = not sent). A nil enabled predicate records unconditionally.
func Wrap(inner llm.Provider, pg *db.DB, model, profName, thinkingType, reasoningEffort string, enabled func() bool) *Recorder {
	return &Recorder{
		inner: inner, pg: pg, model: model, prof: profName,
		thinkingType: thinkingType, reasoningEffort: reasoningEffort, enabled: enabled,
		writeTimeout: 5 * time.Second,
	}
}

// parseSession extracts task id and worker role from session strings like
// "exp1-worker-i3" → ("1", "worker") or "exp2-planner" → ("2", "planner").
func parseSession(s string) (taskID, worker string) {
	// format: exp<N>-<role>[-suffix]
	if !strings.HasPrefix(s, "exp") {
		return "", ""
	}
	rest := s[3:] // after "exp"
	// split task number
	i := strings.IndexByte(rest, '-')
	if i < 0 {
		return rest, ""
	}
	taskID = rest[:i]
	rest = rest[i+1:]
	// worker role is up to the next '-' (e.g. "worker" from "worker-i3")
	if j := strings.IndexByte(rest, '-'); j >= 0 {
		worker = rest[:j]
	} else {
		worker = rest
	}
	return taskID, worker
}

// Stream implements llm.Provider. It delegates to the inner provider, accumulates
// the streamed events to reconstruct the response, and records the full exchange.
//
// The session identifier (e.g. "exp1-worker-i3") is carried on ctx by the norma
// harness via transcript.WithSessionID; reading it per-call is race-free even when
// planner and multiple workers share one Recorder instance.
func (r *Recorder) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	ctx = runtimebudget.WithModelByteLimits(ctx, runtimebudget.ModelBytePolicy(ctx))
	// Body recording (the heavy debug trace: full request/response) is gated by the
	// llm_record setting. Lightweight usage metering always runs — it powers token
	// stats and must be complete even for interrupted/failed runs, so it is NOT
	// gated. Only body serialization + response accumulation are skipped when off.
	recordBodies := r.enabled == nil || r.enabled()
	start := time.Now()
	session := transcript.SessionIDFrom(ctx)
	parsedID, worker := parseSession(session)
	expID := db.ParseExpID(parsedID)
	taskID := TaskIDFrom(ctx)
	runInfo := RunInfoFrom(ctx)
	if runInfo.ExplorationID > 0 {
		expID = runInfo.ExplorationID
	}
	if runInfo.TaskID > 0 {
		taskID = strconv.FormatInt(runInfo.TaskID, 10)
	}
	if runInfo.AgentKey != "" {
		worker = runInfo.AgentKey
	}
	if taskID == "" {
		// Backward compatibility for non-task callers. For task calls, the task
		// runtime always supplies the registry id explicitly.
		taskID = parsedID
	}

	// Serialize the request only when storing bodies (this is the expensive part).
	// Alongside it, attach a Capture so the HTTP transport can hand back the
	// untouched wire bodies — the normalized view below cannot reconstruct them
	// (tool schemas are dropped, tool_use blocks never reach this layer, and the
	// SSE framing is already decoded). See capture.go.
	return func(yield func(llm.StreamEvent, error) bool) {
		callCtx := ctx
		inputErr := callCtx.Err()
		if inputErr == nil {
			inputErr = runtimebudget.CheckModelInputBytes(req, int64(runtimebudget.ModelBytePolicy(callCtx).RequestMiB)*runtimebudget.MiB)
		}
		if inputErr != nil {
			r.recordInputRejection(callCtx, session, taskID, worker, expID, runInfo, start, req.Thinking, inputErr, recordBodies)
			yield(llm.StreamEvent{}, inputErr)
			return
		}
		reqBody := ""
		var capt *Capture
		if recordBodies {
			reqBody = r.serializeRequest(req)
			callCtx, capt = NewCapture(callCtx)
		}
		var (
			textBuf     strings.Builder
			thinkingBuf strings.Builder
			usage       llm.Usage
			stopReason  string
			streamErr   error
		)

		finished := false
		finish := func(err error) {
			if finished {
				return
			}
			finished = true
			writeCtx, cancel := r.writeContext(callCtx)
			defer cancel()
			status := "ok"
			if err != nil {
				status = "error"
			}
			// Lightweight metering row — always written.
			r.recordUsage(writeCtx, taskID, expID, worker, runInfo, req, usage, int(time.Since(start).Milliseconds()), status)
			// Heavy trace row — only when body recording is on.
			if recordBodies {
				r.record(writeCtx, req, session, taskID, worker, reqBody, capt, start, textBuf.String(), thinkingBuf.String(), usage, stopReason, err)
			}
		}
		defer func() { finish(context.Cause(callCtx)) }()

		for ev, err := range r.inner.Stream(callCtx, req) {
			if err != nil {
				streamErr = err
				finish(streamErr)
				if !yield(ev, err) {
					return
				}
				return
			}
			// Always track usage (cheap); accumulate text/thinking only for bodies.
			// Anthropic (and the other providers) split token usage across events:
			// message_start carries ONLY the input side (input + cache), message_delta
			// ONLY the output. They must be FOLDED with Add — overwriting on delta
			// would zero out the input already counted at start (mirrors the SDK's own
			// llm.Accumulator; see norma/llm/accumulate.go).
			switch ev.Type {
			case llm.SETextDelta:
				if recordBodies {
					textBuf.WriteString(ev.Text)
				}
			case llm.SEThinkingDelta:
				if recordBodies {
					thinkingBuf.WriteString(ev.Text)
				}
			case llm.SEMessageStart:
				usage.Add(ev.Usage)
			case llm.SEMessageDelta:
				if ev.StopReason != "" {
					stopReason = ev.StopReason
				}
				usage.Add(ev.Usage)
			}
			if !yield(ev, err) {
				return
			}
		}

		// Stream completed normally.
		finish(streamErr)
	}
}

// Complete implements the atomic completion path while preserving the same
// usage metering, normalized body recording and raw wire capture as Stream.
func (r *Recorder) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	ctx = runtimebudget.WithModelByteLimits(ctx, runtimebudget.ModelBytePolicy(ctx))
	recordBodies := r.enabled == nil || r.enabled()
	start := time.Now()
	session := transcript.SessionIDFrom(ctx)
	parsedID, worker := parseSession(session)
	expID := db.ParseExpID(parsedID)
	taskID := TaskIDFrom(ctx)
	runInfo := RunInfoFrom(ctx)
	if runInfo.ExplorationID > 0 {
		expID = runInfo.ExplorationID
	}
	if runInfo.TaskID > 0 {
		taskID = strconv.FormatInt(runInfo.TaskID, 10)
	}
	if runInfo.AgentKey != "" {
		worker = runInfo.AgentKey
	}
	if taskID == "" {
		taskID = parsedID
	}
	inputErr := ctx.Err()
	if inputErr == nil {
		inputErr = runtimebudget.CheckModelInputBytes(req, int64(runtimebudget.ModelBytePolicy(ctx).RequestMiB)*runtimebudget.MiB)
	}
	if inputErr != nil {
		r.recordInputRejection(ctx, session, taskID, worker, expID, runInfo, start, req.Thinking, inputErr, recordBodies)
		return llm.Message{}, "", llm.Usage{}, inputErr
	}

	reqBody := ""
	var capt *Capture
	if recordBodies {
		reqBody = r.serializeRequest(req)
		ctx, capt = NewCapture(ctx)
	}

	msg, stopReason, usage, err := r.inner.Complete(ctx, req)
	status := "ok"
	if err != nil {
		status = "error"
	}
	writeCtx, cancel := r.writeContext(ctx)
	defer cancel()
	r.recordUsage(writeCtx, taskID, expID, worker, runInfo, req, usage, int(time.Since(start).Milliseconds()), status)
	if recordBodies {
		r.record(writeCtx, req, session, taskID, worker, reqBody, capt, start, msg.Text(), thinkingText(msg), usage, stopReason, err)
	}
	return msg, stopReason, usage, err
}

func thinkingText(msg llm.Message) string {
	var b strings.Builder
	for _, block := range msg.Content {
		if block.Type == llm.BlockThinking && block.Thinking != "" {
			b.WriteString(block.Thinking)
		}
	}
	return b.String()
}

// recordUsage appends one lightweight metering row to llm_usage (no bodies). Skips
// zero-token calls with no model, which carry nothing worth metering.
func (r *Recorder) recordUsage(ctx context.Context, taskID string, expID int64, worker string, runInfo RunInfo, req llm.CompletionRequest, usage llm.Usage, latencyMs int, status string) {
	if r.pg == nil {
		return
	}
	if ctx.Err() != nil {
		log.Printf("[llmusage] record write deadline reached")
		return
	}
	if r.model == "" && usage.InputTokens == 0 && usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 {
		return
	}
	dims := requestDimensionsFor(req)
	phase := runtimebudget.ModelPhase(ctx)
	if phase == "" {
		phase = runInfo.Phase
	}
	if phase == "" {
		phase = "normal"
	}
	var readCalls, repeatedReads *int
	reads, observed := ContextReadMetricsFrom(ctx)
	if rejected, _ := ctx.Value(inputRejectedMetricsKey{}).(bool); rejected {
		dims = requestDimensions{}
		reads = ContextReadMetrics{}
		observed = false
	}
	if observed {
		readCalls, repeatedReads = &reads.Calls, &reads.Repeated
	}
	err := r.pg.InsertLLMUsageContext(ctx, &db.LLMUsage{
		TaskID:               taskID,
		ExplorationID:        expID,
		IntentID:             runInfo.IntentID,
		Worker:               worker,
		Trigger:              runInfo.Trigger,
		RetryOrdinal:         runInfo.RetryOrdinal,
		Phase:                phase,
		Model:                r.model,
		ProfileName:          r.prof,
		LatencyMs:            latencyMs,
		InputTokens:          usage.InputTokens,
		OutputTokens:         usage.OutputTokens,
		CacheRead:            usage.CacheReadTokens,
		CacheWrite:           usage.CacheWriteTokens,
		SystemChars:          dims.system,
		MessageChars:         dims.messages,
		ToolChars:            dims.tools,
		RequestChars:         dims.total,
		EstimatedInputTokens: (dims.total + 3) / 4,
		RequestBytes:         dims.bytes,
		HistoryReadCalls:     readCalls, HistoryRepeatedReads: repeatedReads, HistoryReadTrackingLimited: reads.Limited,
		Status: status,
	})
	if err != nil {
		log.Printf("[llmusage] insert: %v", err)
	}
}

type requestDimensions struct {
	system, messages, tools, total int
	bytes                          int
}

func requestDimensionsFor(req llm.CompletionRequest) requestDimensions {
	var dims requestDimensions
	var sb, mb, tb int
	dims.system, sb = serializedSliceDimensions(req.System)
	dims.messages, mb = serializedSliceDimensions(req.Messages)
	dims.tools, tb = serializedSliceDimensions(req.Tools)
	dims.total = dims.system + dims.messages + dims.tools
	base := req
	base.System = nil
	base.Messages = nil
	base.Tools = nil
	raw, _ := json.Marshal(base)
	dims.bytes = len(raw) - 12 + sb + mb + tb
	return dims
}

// Each element is encoded once for both units; no full-history encoding buffer.
func serializedSliceDimensions[T any](items []T) (int, int) {
	if items == nil {
		return 4, 4
	}
	chars, size := 2, 2
	for i, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			return 0, 0
		}
		chars += utf8.RuneCount(raw)
		size += len(raw)
		if i > 0 {
			chars++
			size++
		}
	}
	return chars, size
}

func serializedSliceRunes[T any](items []T) int {
	chars, _ := serializedSliceDimensions(items)
	return chars
}

// record persists one LLM call to PostgreSQL before the provider stream returns.
// Keeping the write inside the owning task operation means task deletion can
// drain calls and then remove records without a late async insert recreating one.
func (r *Recorder) record(ctx context.Context, req llm.CompletionRequest, session, taskID, worker, reqBody string, capt *Capture, start time.Time, text, thinking string, usage llm.Usage, stopReason string, streamErr error) {
	if r.pg == nil {
		return
	}
	if ctx.Err() != nil {
		log.Printf("[llmrec] record write deadline reached")
		return
	}
	latency := int(time.Since(start).Milliseconds())
	status := "ok"
	errMsg := ""
	if streamErr != nil {
		status = "error"
		errMsg = streamErr.Error()
	}

	// Build response body JSON.
	resp := map[string]any{
		"text":        text,
		"stop_reason": stopReason,
		"usage": map[string]int{
			"input_tokens":       usage.InputTokens,
			"output_tokens":      usage.OutputTokens,
			"cache_read_tokens":  usage.CacheReadTokens,
			"cache_write_tokens": usage.CacheWriteTokens,
		},
	}
	if thinking != "" {
		resp["thinking"] = thinking
	}
	var byteStop *runtimebudget.ModelByteLimitError
	if errors.As(streamErr, &byteStop) {
		resp["incomplete"] = true
		resp["byte_limit"] = map[string]any{"phase": byteStop.Phase, "limit_bytes": byteStop.Limit}
	}
	rawRequest, requestEncoded := storageWireBody(capt.RawRequest())
	receivedResponse := capt.RawResponse()
	rawResponse, responseEncoded := storageWireBody(receivedResponse)
	if byteStop != nil {
		resp["captured_response_bytes"] = len(receivedResponse)
	}
	if requestEncoded {
		resp["raw_request_encoding"] = "base64-envelope"
	}
	if responseEncoded {
		resp["raw_response_encoding"] = "base64-envelope"
	}
	respBody, _ := json.Marshal(resp)

	model := ""
	if len(req.System) > 0 {
		// model is not in CompletionRequest; use the configured model name
	}
	model = r.model

	rec := &db.LLMRecord{
		SessionID:    session,
		TaskID:       taskID,
		Worker:       worker,
		Model:        model,
		ProfileName:  r.prof,
		LatencyMs:    latency,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		CacheRead:    usage.CacheReadTokens,
		CacheWrite:   usage.CacheWriteTokens,
		Status:       status,
		Error:        errMsg,
		RequestBody:  reqBody,
		ResponseBody: string(respBody),
		// Raw wire bodies; empty when the transport did not fill the Capture
		// (e.g. a provider dialing through a client without the capture hook, or
		// a call that failed before any HTTP request went out).
		RawRequest:  rawRequest,
		RawResponse: rawResponse,
	}

	if err := r.pg.InsertLLMRecordContext(ctx, rec); err != nil {
		log.Printf("[llmrec] insert: %v", err)
	}
}

// Oversized projected input was not sent. Record a small diagnostic, not a
// second copy of that input in the recorder or its dimension serializer.
func (r *Recorder) recordInputRejection(ctx context.Context, session, taskID, worker string, expID int64, info RunInfo, start time.Time, thinking string, err error, bodies bool) {
	minimal := llm.CompletionRequest{Thinking: thinking}
	ctx = context.WithValue(ctx, inputRejectedMetricsKey{}, true)
	writeCtx, cancel := r.writeContext(ctx)
	defer cancel()
	r.recordUsage(writeCtx, taskID, expID, worker, info, minimal, llm.Usage{}, int(time.Since(start).Milliseconds()), "error")
	if bodies {
		r.record(writeCtx, minimal, session, taskID, worker, `{"omitted":true,"sent":false,"reason":"model_input_preflight"}`, nil, start, "", "", llm.Usage{}, "", err)
	}
}

// Finish the failed call's diagnostic even after model cancellation, but share
// one finite write budget across usage and body records. Slow PG must not hold
// the model permit indefinitely or cause the model/tools to be replayed.
func (r *Recorder) writeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	limit := r.writeTimeout
	if limit <= 0 {
		limit = 5 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(ctx), limit)
}

// PostgreSQL text cannot store a mid-UTF8 prefix or NUL. Keep received bytes
// losslessly in an explicit base64 envelope instead of losing the whole record
// or replacing bytes. Existing valid wire bodies remain byte-for-byte unchanged.
func storageWireBody(raw string) (string, bool) {
	if utf8.ValidString(raw) && !strings.ContainsRune(raw, 0) {
		return raw, false
	}
	envelope, _ := json.Marshal(map[string]string{"encoding": "base64", "data": base64.StdEncoding.EncodeToString([]byte(raw)), "note": "已提供给SDK的字节前缀无法直接保存为UTF-8文本；解码data可还原该前缀"})
	return string(envelope), true
}

// serializeRequest builds a JSON representation of the completion request.
func (r *Recorder) serializeRequest(req llm.CompletionRequest) string {
	m := map[string]any{
		"system":     req.System,
		"messages":   req.Messages,
		"max_tokens": req.MaxTokens,
	}
	// 记录本次调用实际发出的思考参数。type 采用「有效值」：每请求覆盖 req.Thinking
	// 优先于配置级 thinkingType(与 norma buildBody 的判定一致，如 compaction 摘要会
	// 强制 disabled)；effort 无每请求覆盖，直接取配置值。两者皆空则不写 thinking 字段。
	effType := r.thinkingType
	if req.Thinking != "" {
		effType = req.Thinking
	}
	if effType != "" || r.reasoningEffort != "" {
		m["thinking"] = map[string]string{"type": effType, "effort": r.reasoningEffort}
	}
	if len(req.Tools) > 0 {
		// Store tool names only (full schemas are huge).
		names := make([]string, len(req.Tools))
		for i, t := range req.Tools {
			names[i] = t.Name
		}
		m["tools"] = names
		m["tools_count"] = len(req.Tools)
	}
	if req.Temperature != nil {
		m["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		m["stop"] = req.Stop
	}
	b, _ := json.Marshal(m)
	return string(b)
}
