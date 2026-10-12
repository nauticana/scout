package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/observability"
)

func failingStep(err error) contract.StepExecutorRegistry {
	return fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(context.Context, domain.StepInput) (domain.StepResult, error) {
			return domain.StepResult{}, err
		}), nil
	})
}

func finalFrames(frames []domain.TurnReply) []domain.TurnReply {
	var out []domain.TurnReply
	for _, frame := range frames {
		if frame.Final {
			out = append(out, frame)
		}
	}
	return out
}

func terminalAudits(audits []string) []string {
	var out []string
	for _, category := range audits {
		if category == observability.AuditCategoryTurnFailed || category == observability.AuditCategoryTurnCancelled || category == observability.AuditCategoryTurnCompleted {
			out = append(out, category)
		}
	}
	return out
}

// G2: a failure whose terminal record is refused writes no terminal audit and publishes no final
// frame. Before, fail() audited and published even when Records.Fail refused, so a fenced-out
// worker wrote turn_failed into the middle of another owner's chain.
func TestTurnRuntimeFailWritesNothingTerminalWhenItsRecordIsRefused(t *testing.T) {
	for name, refusal := range map[string]error{
		"fenced out":    fmt.Errorf("%w: turn moved past fence 1", domain.ErrFenceLost),
		"store refused": errors.New("conversation_turn unavailable"),
	} {
		recorder := &runtimeRecorder{}
		runtime := newTestRuntime(t, recorder)
		runtime.Records.(*fake.TurnRecordStore).FailFunc = func(context.Context, domain.TurnOwner, domain.TurnStatus, string) error {
			recorder.record("fail")
			return refusal
		}
		runtime.Executors = failingStep(errors.New("provider failed"))
		if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, refusal) {
			t.Fatalf("%s: error = %v", name, err)
		}
		if got := terminalAudits(recorder.audits); len(got) != 0 {
			t.Fatalf("%s: terminal audit %v written for a refused record", name, got)
		}
		if got := finalFrames(recorder.frames); len(got) != 0 {
			t.Fatalf("%s: final frame %+v published for a refused record", name, got)
		}
	}
}

// §8.1: a worker that lost its fence mid-run, here at its checkpoint, makes no terminal record,
// settlement, audit, or final frame: the turn belongs to whoever moved the fence.
func TestTurnRuntimeStopsWithoutTerminalWritesWhenItsFenceIsLost(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Sessions.(*fake.SessionCoordinator).CheckpointFunc = func(context.Context, domain.TurnOwner, int64, domain.StepCheckpoint) error {
		return fmt.Errorf("%w: turn held", domain.ErrFenceLost)
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("error = %v, want ErrFenceLost", err)
	}
	for _, call := range []string{"fail", "settle", "refund", "usage", "complete", "publish"} {
		if slices.Contains(recorder.calls, call) {
			t.Fatalf("a fenced-out worker made %q: %v", call, recorder.calls)
		}
	}
	if len(recorder.audits) != 0 {
		t.Fatalf("audits = %v", recorder.audits)
	}
}

// A changed fence seen by the control watcher cancels the turn context with ErrFenceLost, which
// ends the delivery with no terminal writes, unlike a cancellation.
func TestTurnRuntimeLeavesATurnWhoseFenceMovedUnderIt(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Control = fencedOutWatcher{}
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(ctx context.Context, _ domain.StepInput) (domain.StepResult, error) {
			<-ctx.Done()
			return domain.StepResult{}, ctx.Err()
		}), nil
	})
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("error = %v, want ErrFenceLost", err)
	}
	if len(recorder.failures) != 0 || recorder.released != 0 || recorder.committed != 0 || len(finalFrames(recorder.frames)) != 0 {
		t.Fatalf("failures %v, refunds %d, settles %d, frames %+v", recorder.failures, recorder.released, recorder.committed, recorder.frames)
	}
}

type fencedOutWatcher struct{}

func (fencedOutWatcher) Watch(ctx context.Context, owner domain.TurnOwner) (contract.TurnWatch, error) {
	turnCtx, cancel := context.WithCancelCause(ctx)
	watch := &memoryTurnWatch{ctx: turnCtx, cancel: cancel, release: func() {}}
	go cancel(fmt.Errorf("%w: turn %q moved to fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence+1))
	return watch, nil
}

// G3: a stale settlement is refused, so the run that owns the turn next still finds a live hold
// instead of a reservation another worker settled under it.
func TestTurnRuntimeStaleSettlementLeavesTheReservationLive(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	budget := runtime.Budget.(*fake.TenantBudgetManager)
	budget.CommitTurnFunc = func(_ context.Context, owner domain.TurnOwner, _ domain.BudgetReservation, _ domain.Usage) error {
		recorder.record("settle-attempt")
		return fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence)
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("error = %v, want ErrFenceLost", err)
	}
	if recorder.committed != 0 || slices.Contains(recorder.calls, "complete") || len(terminalAudits(recorder.audits)) != 0 || len(finalFrames(recorder.frames)) != 0 {
		t.Fatalf("a stale settlement wrote: calls %v, audits %v", recorder.calls, recorder.audits)
	}
	// The next owner reserves again and gets the same live hold, then settles it.
	budget.CommitTurnFunc = nil
	reserved := budget.ReserveFunc
	budget.ReserveFunc = func(ctx context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
		reservation, err := reserved(ctx, request)
		reservation.Replayed = true
		return reservation, err
	}
	runtime.Records.(*fake.TurnRecordStore).AcquireFunc = func(context.Context, domain.TurnClaim) (domain.TurnOwner, error) {
		return domain.TurnOwner{TenantID: 7, RequestID: "request-1", Fence: 2}, nil
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("the next owner: %v", err)
	}
	if recorder.committed != 1 {
		t.Fatalf("the next owner settled %d times, want once against the live hold", recorder.committed)
	}
}

// Every write of one delivery presents the owner its acquisition returned.
func TestTurnRuntimePresentsItsOwnerOnEveryWrite(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	owner := domain.TurnOwner{TenantID: 7, RequestID: "request-1", Fence: 9}
	var seen []string
	check := func(write string, got domain.TurnOwner) {
		if got != owner {
			t.Errorf("%s presented %+v, want %+v", write, got, owner)
		}
		seen = append(seen, write)
	}
	records := runtime.Records.(*fake.TurnRecordStore)
	records.AcquireFunc = func(context.Context, domain.TurnClaim) (domain.TurnOwner, error) { return owner, nil }
	records.RecordUsageFunc = func(_ context.Context, got domain.TurnOwner, _ string, _ int64, _ string, _ domain.UsageAttribution, _ domain.Usage) error {
		check("usage", got)
		return nil
	}
	sessions := runtime.Sessions.(*fake.SessionCoordinator)
	sessions.CheckpointFunc = func(_ context.Context, got domain.TurnOwner, _ int64, _ domain.StepCheckpoint) error {
		check("checkpoint", got)
		return nil
	}
	sessions.CompleteFunc = func(_ context.Context, got domain.TurnOwner, _ string, _ int64, _ domain.TurnResult) error {
		check("complete", got)
		return nil
	}
	runtime.Idempotency = &fake.StepIdempotencyStore{
		BeginFunc: func(_ context.Context, got domain.TurnOwner, _ domain.ExecutionStep) (domain.StepResult, bool, error) {
			check("step begin", got)
			return domain.StepResult{}, false, nil
		},
		CommitFunc: func(_ context.Context, got domain.TurnOwner, _ domain.ExecutionStep, _ domain.StepResult) error {
			check("step commit", got)
			return nil
		},
	}
	runtime.Budget.(*fake.TenantBudgetManager).CommitTurnFunc = func(_ context.Context, got domain.TurnOwner, _ domain.BudgetReservation, _ domain.Usage) error {
		check("budget settle", got)
		return nil
	}
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(_ context.Context, input domain.StepInput) (domain.StepResult, error) {
			check("step input", input.Owner)
			return stepExecutor(recorder).Execute(context.Background(), input)
		}), nil
	})
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatal(err)
	}
	for _, write := range []string{"step begin", "step input", "step commit", "checkpoint", "budget settle", "usage", "complete"} {
		if !slices.Contains(seen, write) {
			t.Errorf("%s did not present the owner", write)
		}
	}
	for _, frame := range recorder.frames {
		if frame.Fence != owner.Fence {
			t.Errorf("frame %d published under fence %d, want %d", frame.Sequence, frame.Fence, owner.Fence)
		}
	}
}

// §6, boundary 1: a hold requested while a step runs is taken once that step is checkpointed
// and its frame published. The turn is held under a new fence, the reservation is kept, the
// delivery ends on a final held frame, and the next step does not run.
func TestTurnRuntimeHoldsAtTheNextStepCheckpointWhenAsked(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	control := &MemoryTurnControl{}
	runtime.Control = control
	var held []domain.TurnOwner
	runtime.Records.(*fake.TurnRecordStore).HoldFunc = func(_ context.Context, owner domain.TurnOwner) (int64, error) {
		recorder.record("hold")
		held = append(held, owner)
		return owner.Fence + 1, nil
	}
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
			if err := control.RequestHold(ctx, 7, "request-1"); err != nil {
				t.Errorf("RequestHold: %v", err)
			}
			return stepExecutor(recorder).Execute(ctx, input)
		}), nil
	})
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("a hold acks the delivery: %v", err)
	}
	if !slices.Equal(recorder.executed, []string{"step-1"}) || len(held) != 1 || held[0] != runtimeOwner {
		t.Fatalf("executed %v, held %+v", recorder.executed, held)
	}
	if callIndex(recorder.calls, "checkpoint") > callIndex(recorder.calls, "hold") {
		t.Fatalf("the hold must follow the step's checkpoint: %v", recorder.calls)
	}
	if recorder.committed != 0 || recorder.released != 0 || len(recorder.failures) != 0 || slices.Contains(recorder.calls, "complete") {
		t.Fatalf("a hold settles nothing: calls %v", recorder.calls)
	}
	last := recorder.frames[len(recorder.frames)-1]
	if len(recorder.frames) != 2 || !last.Final || last.Sequence != 1 || last.Fence != 2 || last.ErrorCode != "held" ||
		len(last.Events) != 1 || last.Events[0].Kind != domain.TurnEventHeld {
		t.Fatalf("frames = %+v", recorder.frames)
	}
	if !slices.Equal(recorder.audits, []string{observability.AuditCategoryTurnHeld}) {
		t.Fatalf("audits = %v", recorder.audits)
	}
}

// A hold taken inside a step, here by a tool loop at one of its boundaries, abandons the step's
// claim under the owner's fence first, so the released turn replays it from its journal.
func TestTurnRuntimeAbandonsTheStepItHoldsInside(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	var order []string
	runtime.Idempotency = &fake.StepIdempotencyStore{AbandonFunc: func(context.Context, domain.TurnOwner, domain.ExecutionStep) error {
		order = append(order, "abandon")
		return nil
	}}
	runtime.Records.(*fake.TurnRecordStore).HoldFunc = func(_ context.Context, owner domain.TurnOwner) (int64, error) {
		order = append(order, "hold")
		return owner.Fence + 1, nil
	}
	spent := domain.Usage{OutputTokens: 7, Currency: "USD"}
	runtime.Executors = failingStep(&LoopError{Err: fmt.Errorf("%w: before call %q", domain.ErrTurnHeld, "c1"), Usage: spent})
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("a hold acks the delivery: %v", err)
	}
	if !slices.Equal(order, []string{"abandon", "hold"}) || recorder.committed != 0 || recorder.released != 0 || len(recorder.usage) != 0 {
		t.Fatalf("order %v, settles %d, refunds %d, usage %v", order, recorder.committed, recorder.released, recorder.usage)
	}
}

// A current delivery that finds its turn held or suspended runs nothing and is acknowledged.
func TestTurnRuntimeRunsNothingForAHeldTurn(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Records.(*fake.TurnRecordStore).AcquireFunc = func(context.Context, domain.TurnClaim) (domain.TurnOwner, error) {
		return domain.TurnOwner{}, fmt.Errorf("%w: turn request-1 is H", domain.ErrTurnHeld)
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, domain.ErrTurnHeld) {
		t.Fatalf("error = %v", err)
	}
	if len(recorder.calls) != 0 || len(recorder.audits) != 0 {
		t.Fatalf("a held turn ran: %v, %v", recorder.calls, recorder.audits)
	}
}

// §8.10 for hold and release: admitted, held, released, then completed is a gap-free chain, the
// held fence and the released one each recorded once and carried in the turn audit payload.
func TestTurnChainVerifiesThroughAHoldAndARelease(t *testing.T) {
	harness := newChainHarness(t)
	ctx := context.Background()
	if _, err := harness.ingress.OpenTurn(ctx, runtimeDispatch().Turn); err != nil {
		t.Fatal(err)
	}
	control := &MemoryTurnControl{}
	harness.runtime.Control = control
	harness.runtime.Records.(*fake.TurnRecordStore).AcquireFunc = func(context.Context, domain.TurnClaim) (domain.TurnOwner, error) {
		return runtimeOwner, nil
	}
	harness.runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
			_ = control.RequestHold(ctx, 7, "request-1")
			return stepExecutor(&runtimeRecorder{}).Execute(ctx, input)
		}), nil
	})
	if _, err := harness.runtime.HandleTurn(ctx, runtimeClaim()); err != nil {
		t.Fatal(err)
	}
	actor := domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}
	if err := auditTurnControl(ctx, harness.sink, actor, "agent", "request-1", observability.AuditCategoryTurnReleased, 3, "released", time.Unix(1_700_000_000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	// A retried release request at the same fence is the same decision.
	if err := auditTurnControl(ctx, harness.sink, actor, "agent", "request-1", observability.AuditCategoryTurnReleased, 3, "released", time.Unix(1_700_000_000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	harness.runtime.Records.(*fake.TurnRecordStore).AcquireFunc = func(context.Context, domain.TurnClaim) (domain.TurnOwner, error) {
		return domain.TurnOwner{TenantID: 7, RequestID: "request-1", Fence: 4}, nil
	}
	harness.runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return stepExecutor(&runtimeRecorder{}), nil
	})
	if _, err := harness.runtime.HandleTurn(ctx, runtimeClaim()); err != nil {
		t.Fatal(err)
	}
	chain := harness.chain()
	want := []string{observability.AuditCategoryTurnAdmitted, observability.AuditCategoryTurnHeld, observability.AuditCategoryTurnReleased, observability.AuditCategoryTurnCompleted}
	if got := categories(chain); !slices.Equal(got, want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	if err := observability.VerifyTurnChain(chain); err != nil {
		t.Fatalf("VerifyTurnChain: %v", err)
	}
	var held struct {
		Fence int64 `json:"fence"`
	}
	if err := json.Unmarshal(chain[1].Payload, &held); err != nil || held.Fence != 2 {
		t.Fatalf("held payload = %s, %v", chain[1].Payload, err)
	}
}

// A cancellation requested just before the worker takes its hold wins: the turn ends cancelled
// with its usage settled, and no cancelled turn is parked where the canceller cannot reach it.
func TestTurnRuntimeFailsACancelledTurnInsteadOfHoldingIt(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	control := &MemoryTurnControl{}
	runtime.Control = control
	runtime.Records.(*fake.TurnRecordStore).HoldFunc = func(context.Context, domain.TurnOwner) (int64, error) {
		recorder.record("hold")
		return 0, fmt.Errorf("%w: operator asked", domain.ErrTurnCanceled)
	}
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
			if err := control.RequestHold(ctx, 7, "request-1"); err != nil {
				t.Errorf("RequestHold: %v", err)
			}
			return stepExecutor(recorder).Execute(ctx, input)
		}), nil
	})
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, domain.ErrTurnCanceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if !slices.Equal(recorder.failures, []string{string(domain.TurnCancelled) + ":canceled"}) || recorder.committed != 1 {
		t.Fatalf("failures %v, settled %d: the cancelled turn settles its usage and ends X", recorder.failures, recorder.committed)
	}
	if slices.Contains(recorder.audits, observability.AuditCategoryTurnHeld) || !slices.Contains(recorder.audits, observability.AuditCategoryTurnCancelled) {
		t.Fatalf("audits = %v", recorder.audits)
	}
	last := recorder.frames[len(recorder.frames)-1]
	if !last.Final || last.ErrorCode != "canceled" || slices.ContainsFunc(last.Events, func(event domain.TurnEvent) bool { return event.Kind == domain.TurnEventHeld }) {
		t.Fatalf("final frame = %+v", last)
	}
}
