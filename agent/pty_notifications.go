package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// Result.Extra is a public SDK hook for real, additional conversation messages.
// The SDK pairs and records these after the tool batch, including deferred calls.
// It is NOT a synthetic tool call, polling request or replay of a completed tool.
type ptyNoticeTool struct {
	actool.CoreTool
	owner *ptyOwner
}

func (t *ptyNoticeTool) PrepareInput(in json.RawMessage) (json.RawMessage, error) {
	return PrepareToolInput(t.CoreTool, in)
}
func (t *ptyNoticeTool) AcceptsRawInput() bool {
	v, ok := t.CoreTool.(interface{ AcceptsRawInput() bool })
	return ok && v.AcceptsRawInput()
}
func (t *ptyNoticeTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	r, err := t.CoreTool.Call(ctx, in, tc)
	if ctx.Err() == nil {
		if notes := t.owner.drain(); len(notes) > 0 {
			r.Extra = append(r.Extra, ptyNotificationMessage(notes))
		}
	}
	return r, err
}

func ptyNotificationMessage(notes []actool.Notification) llm.Message {
	var out strings.Builder
	for i, n := range notes {
		if i >= 8 {
			break
		}
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(n.String())
	}
	if len(notes) > 8 {
		out.WriteString("\n<pty-status-index>其他会话的精简状态；用shell_read读取详情，状态不是执行证据。\n")
		for _, n := range notes[8:] {
			fmt.Fprintf(&out, "%s: %s\n", n.TaskID, n.Status)
		}
		out.WriteString("</pty-status-index>")
	}
	return llm.UserText(out.String())
}

// The official harness owns each model/tool loop. A late PTY event can land
// while its final answer is streaming; only a real pending event continues the
// same Prompt through another public Query. No session-start/user hook, user
// prompt, tool or summary is replayed. Original turn/time/token budgets and
// cumulative usage continue across these segments; only one terminal is exposed.
func (s *hostedSession) runPTYQuery(ctx context.Context, q harness.QueryInput, deps harness.QueryDeps, yield func(harness.Event, error) bool) {
	started := time.Now()
	var usage llm.Usage
	turnOffset := 0 // SDK starts every Query at turn 1; continuations reuse it.
	if deps.NewID == nil {
		var ids atomic.Int64
		deps.NewID = func() string { return fmt.Sprintf("id_%d", ids.Add(1)) }
	}
	for {
		current := q
		current.Messages = s.messages
		if q.MaxTurns > 0 {
			current.MaxTurns = q.MaxTurns - turnOffset
		}
		if q.MaxDuration > 0 {
			current.MaxDuration = max(time.Nanosecond, q.MaxDuration-time.Since(started))
		}
		if q.TokenBudget > 0 {
			current.TokenBudget = max(1, q.TokenBudget-usage.OutputTokens)
		}
		var terminal *harness.Terminal
		var terminalErr error
		for event, err := range harness.Query(ctx, current, deps) {
			if event.Kind == harness.KindResult && event.Terminal != nil {
				copy := *event.Terminal
				terminal = &copy
				terminalErr = err
				s.messages = append([]llm.Message(nil), llm.MessagesAfterBoundary(copy.Messages)...)
				continue
			}
			if event.Usage != nil {
				combined := usage
				combined.Add(*event.Usage)
				event.Usage = &combined
			}
			if !yield(event, err) {
				return
			}
		}
		if terminal == nil {
			return
		}
		usage.Add(terminal.Usage)
		terminal.Turns += turnOffset
		terminal.Usage = usage
		s.usage = usage
		if ctx.Err() == nil && terminal.Err == nil && terminal.Reason == harness.ReasonCompleted {
			if notes := s.ptys.drain(); len(notes) > 0 {
				s.record(ptyNotificationMessage(notes))
				if q.MaxTurns == 0 || terminal.Turns <= q.MaxTurns {
					turnOffset = terminal.Turns - 1
					continue
				}
				// Preserve the event for the next Prompt even if the turn budget
				// has no room. Zero MaxTurns would mean unlimited to the SDK.
				terminal.Reason = harness.ReasonMaxTurns
			}
		}
		terminal.Messages = s.messages
		yield(harness.Event{Kind: harness.KindResult, Terminal: terminal}, terminalErr)
		return
	}
}
