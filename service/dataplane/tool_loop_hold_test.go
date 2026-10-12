package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

// §6, boundary 3: a requested hold stops the loop after the decision is journaled and before a
// mutating call leaves; a read-only call of the same decision still runs. The resumed loop
// replays the decision and the observation and runs only the held call, once.
func TestToolLoopHoldsBeforeAMutatingCallLeaves(t *testing.T) {
	harness := newLoopHarness(t,
		proposes(10, toolCall("a", "search", `{"q":"x"}`), toolCall("b", "shop_write", `{"sku":"1"}`)),
		answers("done"))
	held := loopInput()
	held.HoldRequested = func() bool { return true }
	if _, err := harness.executor().Execute(context.Background(), held); !errors.Is(err, domain.ErrTurnHeld) {
		t.Fatalf("want ErrTurnHeld, got %v", err)
	}
	if len(harness.calls) != 1 || harness.calls[0].ToolID != "search" || harness.modelCalls != 1 {
		t.Fatalf("before the hold: calls %+v, model calls %d", harness.calls, harness.modelCalls)
	}
	journal, _ := harness.journal.Load(context.Background(), domain.LoopKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11})
	if len(journal) != 2 || journal[0].Kind != domain.LoopEntryModel || journal[1].Kind != domain.LoopEntryObservation {
		t.Fatalf("journal at the hold = %+v", journal)
	}
	result, err := harness.executor().Execute(context.Background(), loopInput())
	if err != nil || string(result.State) != "done" {
		t.Fatalf("released: %s, %v", result.State, err)
	}
	if harness.modelCalls != 2 || len(harness.calls) != 2 || harness.calls[1].ToolID != "shop.write" {
		t.Fatalf("after release: calls %+v, model calls %d", harness.calls, harness.modelCalls)
	}
	for key, count := range harness.effects {
		if count != 1 {
			t.Fatalf("effect %s ran %d times", key, count)
		}
	}
}

// §6, boundary 2: a hold requested during an iteration is taken once its observations are
// journaled, before the model is asked again.
func TestToolLoopHoldsBetweenIterations(t *testing.T) {
	harness := newLoopHarness(t, proposes(10, toolCall("a", "search", `{"q":"x"}`)), answers("done"))
	requested := false
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		requested = true
		return domain.ToolResult{}, nil
	}
	input := loopInput()
	input.HoldRequested = func() bool { return requested }
	if _, err := harness.executor().Execute(context.Background(), input); !errors.Is(err, domain.ErrTurnHeld) {
		t.Fatalf("want ErrTurnHeld, got %v", err)
	}
	if harness.modelCalls != 1 || len(harness.calls) != 1 {
		t.Fatalf("the model was asked again past the hold: model calls %d", harness.modelCalls)
	}
	harness.invoke = nil
	if result, err := harness.executor().Execute(context.Background(), loopInput()); err != nil || string(result.State) != "done" || len(harness.calls) != 1 {
		t.Fatalf("released: %s, %v, calls %d", result.State, err, len(harness.calls))
	}
}

// A loop whose owner was fenced out journals nothing: its decision is refused.
func TestToolLoopJournalsNothingForAFencedOutOwner(t *testing.T) {
	harness := newLoopHarness(t, answers("done"))
	stale := loopInput()
	stale.Owner.Fence = 0
	if _, err := harness.executor().Execute(context.Background(), stale); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("want ErrFenceLost, got %v", err)
	}
	journal := harness.journal.(*MemoryLoopJournal)
	journal.Fences.Hold(loopOwner.TenantID, loopOwner.RequestID)
	if _, err := newLoopHarness(t, answers("done")).executor().Execute(context.Background(), loopInput()); err != nil {
		t.Fatalf("an owner of a fresh journal: %v", err)
	}
	harness = &loopHarness{t: t, script: []domain.ModelResult{answers("done")}, effects: map[string]int{}, journal: journal, limits: harness.limits}
	if _, err := harness.executor().Execute(context.Background(), loopInput()); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("an owner fenced out by a hold = %v, want ErrFenceLost", err)
	}
}
