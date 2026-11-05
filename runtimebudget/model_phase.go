package runtimebudget

import "context"

type modelPhaseKey struct{}

// A host operation supplies its purpose separately from the model's thinking
// setting. Disabling thinking is not proof that a call is a compression call.
func WithModelPhase(ctx context.Context, phase string) context.Context {
	return context.WithValue(ctx, modelPhaseKey{}, phase)
}

func ModelPhase(ctx context.Context) string {
	phase, _ := ctx.Value(modelPhaseKey{}).(string)
	return phase
}
