package toolgateway

import (
	"context"
	"fmt"
	"time"

	"github.com/nauticana/keel/clock"

	"github.com/nauticana/scout/domain"
)

// reconcile observes before the mutation. A satisfied effect is returned as the call's
// result without invoking the transport; an unobservable one refuses the call.
func (gateway *GovernedGateway) reconcile(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) (domain.ToolResult, bool, error) {
	if !definition.VerifyEffect {
		return domain.ToolResult{}, false, nil
	}
	observation, err := gateway.observe(ctx, call, definition, nil)
	switch observation.Status {
	case domain.EffectSatisfied:
		observation.Reconciled = true
		return domain.ToolResult{Output: observation.Output, Effect: &observation}, true, nil
	case domain.EffectViolated:
		return domain.ToolResult{}, false, nil
	default:
		return domain.ToolResult{Effect: &observation}, false, err
	}
}

// verifyEffect observes after an accepted mutation. Only a satisfied effect keeps the
// output; the usage of a violated or unknown one is still reported.
func (gateway *GovernedGateway) verifyEffect(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, result domain.ToolResult) (domain.ToolResult, error) {
	if !definition.VerifyEffect {
		return result, nil
	}
	observation, err := gateway.observe(ctx, call, definition, &result)
	switch observation.Status {
	case domain.EffectSatisfied:
		result.Effect = &observation
		return result, nil
	case domain.EffectViolated:
		err = fmt.Errorf("%w: tool %q: %s", domain.ErrEffectViolated, call.ToolID, observation.Reason)
	}
	return domain.ToolResult{Usage: result.Usage, Effect: &observation}, err
}

// observe downgrades to unknown an observation that failed, is unrecognized, or rests on no
// evidence; only "not there yet" before the mutation needs none. The returned error is set
// exactly when the status is unknown.
func (gateway *GovernedGateway) observe(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, result *domain.ToolResult) (domain.EffectObservation, error) {
	observation, err := gateway.Effects.Observe(ctx, call, definition, result)
	known := observation.Status == domain.EffectSatisfied || observation.Status == domain.EffectViolated
	switch {
	case err != nil:
		observation = domain.EffectObservation{Reason: "the effect could not be observed"}
		err = fmt.Errorf("%w: tool %q: %w", domain.ErrEffectUnknown, call.ToolID, err)
	case known && len(observation.Evidence) == 0 && (result != nil || observation.Status == domain.EffectSatisfied):
		observation = domain.EffectObservation{Reason: "the observation carries no evidence"}
		err = fmt.Errorf("%w: tool %q: %s", domain.ErrEffectUnknown, call.ToolID, observation.Reason)
	case !known:
		err = fmt.Errorf("%w: tool %q: %s", domain.ErrEffectUnknown, call.ToolID, observation.Reason)
	}
	if err != nil {
		observation.Status, observation.Output = domain.EffectUnknown, nil
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = gateway.now()
	}
	return observation, err
}

func (gateway *GovernedGateway) now() time.Time {
	if gateway.Clock == nil {
		return clock.System{}.Now().UTC()
	}
	return gateway.Clock.Now().UTC()
}
