package dataplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/observability"
)

func unknownOutcomeStep(abandoned *[]domain.TurnOwner) contract.StepExecutorRegistry {
	return fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(context.Context, domain.StepInput) (domain.StepResult, error) {
			return domain.StepResult{}, &LoopError{Err: fmt.Errorf("%w: call %q: %w", domain.ErrOutcomeUnknown, "b", domain.ErrEffectUnknown),
				Usage: domain.Usage{InputTokens: 10, Currency: "USD"}}
		}), nil
	})
}

// Decision 2: a call whose outcome is unknown holds the turn: held under code U with its fence
// moved, a final held frame, the reservation kept and nothing settled, the step's claim given
// back, and turn_held audited with the reason.
func TestTurnRuntimeAutoHoldsOnAnUnknownOutcome(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Executors = unknownOutcomeStep(nil)
	var codes []domain.TurnHoldCode
	runtime.Records.(*fake.TurnRecordStore).HoldFunc = func(_ context.Context, owner domain.TurnOwner, code domain.TurnHoldCode) (int64, error) {
		codes = append(codes, code)
		return owner.Fence + 1, nil
	}
	var abandoned int
	runtime.Idempotency = &fake.StepIdempotencyStore{AbandonFunc: func(context.Context, domain.TurnOwner, domain.ExecutionStep) error {
		abandoned++
		return nil
	}}
	var reasons []string
	runtime.Audit = &fake.AuditSink{RecordFunc: func(_ context.Context, record domain.DecisionRecord) error {
		recorder.audits = append(recorder.audits, record.Category)
		reasons = append(reasons, record.Reason)
		return nil
	}}
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("an auto-hold ends the delivery cleanly: %v", err)
	}
	if !slices.Equal(codes, []domain.TurnHoldCode{domain.HoldUnknownOutcome}) || abandoned != 1 {
		t.Fatalf("hold codes %v, abandoned %d", codes, abandoned)
	}
	if recorder.committed != 0 || recorder.released != 0 || len(recorder.failures) != 0 || len(recorder.usage) != 0 {
		t.Fatalf("an auto-hold settled: committed %d, refunded %d, failures %v", recorder.committed, recorder.released, recorder.failures)
	}
	frames := finalFrames(recorder.frames)
	if len(frames) != 1 || frames[0].ErrorCode != "held" || frames[0].Fence != 2 || frames[0].Events[0].Kind != domain.TurnEventHeld {
		t.Fatalf("final frames = %+v", frames)
	}
	if !slices.Equal(recorder.audits, []string{observability.AuditCategoryTurnHeld}) || reasons[0] != "outcome_unknown" {
		t.Fatalf("audits %v, reasons %v", recorder.audits, reasons)
	}
}

// A cancellation requested before the auto-hold wins: the turn ends cancelled, and the call's
// record stays unknown for reconciliation.
func TestTurnRuntimeCancellationWinsOverAnAutoHold(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Executors = unknownOutcomeStep(nil)
	runtime.Records.(*fake.TurnRecordStore).HoldFunc = func(context.Context, domain.TurnOwner, domain.TurnHoldCode) (int64, error) {
		return 0, fmt.Errorf("%w: user stopped it", domain.ErrTurnCanceled)
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, domain.ErrTurnCanceled) {
		t.Fatalf("error = %v", err)
	}
	if len(recorder.failures) != 1 || recorder.failures[0] != string(domain.TurnCancelled)+":canceled" {
		t.Fatalf("failures = %v", recorder.failures)
	}
}
