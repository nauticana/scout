package dataplane

import (
	"context"
	"fmt"
	"testing"

	"github.com/nauticana/scout/domain"
)

// A violated effect reaches the model as a failure class and the UI as a typed event, and
// redelivery replays the journaled observation instead of calling the tool again.
func TestToolLoopJournalsTheObservedEffectAndNeverReportsAViolationAsSuccess(t *testing.T) {
	harness := newLoopHarness(t, proposes(10, toolCall("a", "shop_write", `{"sku":"1"}`)), answers("could not update"))
	observed := domain.EffectObservation{Status: domain.EffectViolated, Reason: "old value", Evidence: []domain.ObjectRef{{URI: "memory://observed/1", Digest: "d1"}}}
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Effect: &observed}, fmt.Errorf("%w: old value", domain.ErrEffectViolated)
	}
	effectEvents := func(result domain.StepResult) (events []domain.TurnEffectEvent) {
		for _, event := range result.Events {
			if event.Kind == domain.TurnEventEffect {
				events = append(events, *event.Effect)
			}
		}
		return events
	}
	result, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	seen := harness.lastRequest.Messages[1].Observations[0]
	if !seen.IsError || string(seen.Output) != `{"error":"effect_violated"}` {
		t.Fatalf("the model saw %+v", seen)
	}
	if events := effectEvents(result); len(events) != 1 || events[0].CallID != "a" || events[0].Observation.Status != domain.EffectViolated || len(events[0].Observation.Evidence) != 1 {
		t.Fatalf("effect events = %+v", events)
	}

	harness.modelCalls = 0
	replayed, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil || len(harness.calls) != 1 || len(effectEvents(replayed)) != 1 {
		t.Fatalf("replay must keep the evidence without a second call: %v, calls %d, events %+v", err, len(harness.calls), effectEvents(replayed))
	}
}
