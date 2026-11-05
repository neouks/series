package server

import (
	"math"
	"strings"

	"github.com/neouks/series/agent"
)

const plannerNotificationLimit = 64

// Notifications are wake-up hints, not the durable change log or constraints.
// Keep a bounded recent sample; business changes are journaled by database
// triggers in the mutation transaction, even if the process dies before Notify.
func (t *Task) enqueuePlannerTrigger(event agent.TriggerEvent) {
	budget := 4096
	preview := func(text string) string {
		count := 0
		for index := range text {
			if count == budget {
				budget = 0
				event.Truncated = true
				return strings.Clone(text[:index])
			}
			count++
		}
		budget -= count
		return strings.Clone(text)
	}
	event.Kind = preview(event.Kind)
	if len(event.Kind) > 80 {
		event.Kind = "change"
	}
	event.Detail = preview(event.Detail)
	event.OldGoal = preview(event.OldGoal)
	event.NewGoal = preview(event.NewGoal)
	texts := func(values []string) []string {
		out := []string{}
		for _, text := range values {
			if budget == 0 || len(out) == 16 {
				event.Truncated = true
				break
			}
			out = append(out, preview(text))
		}
		return out
	}
	event.Goals = texts(event.Goals)
	event.Hints = texts(event.Hints)
	ids := make([]int64, 0, min(64, len(event.NodeIDs)))
	seen := map[int64]bool{}
	for _, id := range event.NodeIDs {
		if id <= 0 || seen[id] {
			continue
		}
		if len(ids) == 64 {
			event.Truncated = true
			break
		}
		ids = append(ids, id)
		seen[id] = true
	}
	event.NodeIDs = ids
	event.Kind = strings.TrimSpace(event.Kind)
	event.Count = max(1, event.Count)
	t.trigMu.Lock()
	defer t.trigMu.Unlock()
	if len(t.pendingTriggers) >= plannerNotificationLimit {
		count := max(1, t.pendingTriggers[0].Count)
		t.triggerOverflow = min(math.MaxInt64-count, t.triggerOverflow) + count
		copy(t.pendingTriggers, t.pendingTriggers[1:])
		t.pendingTriggers = t.pendingTriggers[:plannerNotificationLimit-1]
	}
	t.pendingTriggers = append(t.pendingTriggers, event)
}

func (t *Task) restorePlannerTriggers(events []agent.TriggerEvent) {
	for _, event := range events {
		t.enqueuePlannerTrigger(event)
	}
}
