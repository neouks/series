package agent

import (
	"context"
	"iter"
	"reflect"

	"github.com/Autumn-27/norma/llm"
	"github.com/neouks/series/runtimebudget"
)

type incompleteModelTools struct{}

func (*incompleteModelTools) Error() string {
	return "模型工具批次未完整结束，本批工具未执行；请检查模型连接后明确恢复"
}
func (*incompleteModelTools) PreventModelReplay() bool { return true }

var pendingModelEventBytes = int64(reflect.TypeOf(llm.StreamEvent{}).Size()) * 2

// The official streaming executor may finalize/run a tool before the stream
// ends, including after a terminal read error. Buffer from the first tool event
// until successful completion, preserving exact event order/signatures/usage.
// Text before that point remains live. No SDK sources or saved history change.
func committedModelStream(ctx context.Context, req llm.CompletionRequest, stream func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		callCtx, release, err := acquireModelRequest(ctx, runtimebudget.ModelGate(), false)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		defer release()
		limit := int64(runtimebudget.ModelBytePolicy(callCtx).ResponseMiB) * runtimebudget.MiB
		var pending []llm.StreamEvent
		var size int64
		buffering, stopped := false, false
		terminalReason := ""
		var failure error
		for event, err := range stream(callCtx, req) {
			if err != nil {
				failure = err
				break
			}
			buffering = buffering || event.Type == llm.SEToolUseStart || event.Type == llm.SEToolInputJSON || event.Type == llm.SEToolUseSignature
			if !buffering {
				if !yield(event, nil) {
					return
				}
				if event.Type == llm.SEMessageStop {
					break
				}
				continue
			}
			size += pendingModelEventBytes + int64(len(event.Type)+len(event.Text)+len(event.ToolID)+len(event.ToolName)+len(event.StopReason))
			if size > limit {
				failure = &runtimebudget.ModelByteLimitError{Phase: "tool_batch", Limit: limit}
				if owner, _ := callCtx.Value(modelDeadlineKey{}).(*modelCallDeadline); owner != nil {
					owner.cancel(failure)
				}
				break
			}
			pending = append(pending, event)
			if event.Type == llm.SEMessageDelta && event.StopReason != "" {
				terminalReason = event.StopReason
			}
			stopped = stopped || event.Type == llm.SEMessageStop
			if stopped {
				break
			} // Protocol completion, not an unbounded socket tail.
		}
		failure = modelRequestError(callCtx, failure)
		if failure == nil && callCtx.Err() != nil {
			failure = callCtx.Err()
		}
		// Responses emits a terminal message_delta but no message_stop. Accept
		// its clean EOF only after a successful stop reason; a later read error
		// still rejects the batch. Do not commit token-truncated tool arguments.
		terminal := terminalReason == "end_turn" || terminalReason == "tool_use"
		if failure == nil && buffering && ((!stopped && !terminal) || terminalReason == "max_tokens") {
			failure = &incompleteModelTools{}
		}
		if failure != nil {
			pending = nil
			yield(llm.StreamEvent{}, failure)
			return
		}
		// Network work is complete. Disarm its timers before replaying committed
		// events; tools use their own task/run context and execution deadlines.
		if owner, _ := callCtx.Value(modelDeadlineKey{}).(*modelCallDeadline); owner != nil {
			owner.cleanup()
		}
		for _, event := range pending {
			if !yield(event, nil) {
				return
			}
		}
	}
}
