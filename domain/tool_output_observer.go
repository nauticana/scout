package domain

import "context"

type toolOutputObserverKey struct{}

// WithToolOutputObserver asks the governed gateway to hand observe a successful call's validated
// output before tool-output guardrails rewrite it, for a caller that reads the tool's declared
// outcome, such as a Charter binding; the guarded result is still what the gateway returns.
func WithToolOutputObserver(ctx context.Context, observe func(output []byte)) context.Context {
	return context.WithValue(ctx, toolOutputObserverKey{}, observe)
}

// ObserveToolOutput hands output to the context's observer, if any.
func ObserveToolOutput(ctx context.Context, output []byte) {
	if observe, ok := ctx.Value(toolOutputObserverKey{}).(func([]byte)); ok {
		observe(output)
	}
}
