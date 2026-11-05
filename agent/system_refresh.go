package agent

import "github.com/Autumn-27/norma/llm"

// Remove only caller-owned dynamic segments. Surviving cacheable segments keep
// their original status; later dynamic content is never promoted into the
// static prefix. No matches means no slice copy. The source request is immutable.
func withoutSystemSegments(req llm.CompletionRequest, drop func(string) bool) llm.CompletionRequest {
	first := -1
	for i, text := range req.System {
		if drop(text) {
			first = i
			break
		}
	}
	if first < 0 {
		// Callers append refreshed state next. Clip spare capacity so that
		// append cannot write through a shared base prompt's backing array.
		req.System = req.System[:len(req.System):len(req.System)]
		return req
	}
	oldBoundary := min(max(0, req.DynamicBoundary), len(req.System))
	keptCached := min(first, oldBoundary)
	system := make([]string, 0, len(req.System))
	system = append(system, req.System[:first]...)
	for i := first + 1; i < len(req.System); i++ {
		if !drop(req.System[i]) {
			system = append(system, req.System[i])
			if i < oldBoundary {
				keptCached++
			}
		}
	}
	req.System = system
	if req.DynamicBoundary > 0 {
		req.DynamicBoundary = keptCached
	}
	return req
}
