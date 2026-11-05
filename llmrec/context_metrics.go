package llmrec

import "context"

type contextReadMetricsKey struct{}

// ContextReadMetrics describes tool-use blocks in this model request, not new
// tool executions. Replaying a history in another request counts it again here.
// The execution ledger remains the source of actual invocation totals.
type ContextReadMetrics struct {
	Calls, Repeated int
	Limited         bool
}

func WithContextReadMetrics(ctx context.Context, metrics ContextReadMetrics) context.Context {
	return context.WithValue(ctx, contextReadMetricsKey{}, metrics)
}

// ContextReadMetricsFrom reports whether this call has an observed model view.
func ContextReadMetricsFrom(ctx context.Context) (ContextReadMetrics, bool) {
	m, ok := ctx.Value(contextReadMetricsKey{}).(ContextReadMetrics)
	return m, ok
}
