package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/keel/cache"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/observability"
)

// §2: preempt holds a running turn at once under a new fence and code P, so the running worker's
// watch fences it out; a held turn reports held, and a cancellation requested first wins.
func TestTableTurnControlPreemptFencesOutARunningWorker(t *testing.T) {
	table := newControlTable(domain.TurnRunning, 4)
	shared := cache.NewMemoryCacheService()
	worker := &TableTurnControl{DB: table, Cache: shared, PollInterval: time.Hour}
	defer worker.Close()
	watch, err := worker.Watch(context.Background(), domain.TurnOwner{TenantID: 7, RequestID: "request-1", Fence: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()
	operator := &TableTurnControl{DB: table, Cache: shared}
	outcome, err := operator.Preempt(context.Background(), 7, "request-1")
	if err != nil || !outcome.Held() || outcome.Fence != 5 || table.holdCode != domain.HoldPreempted {
		t.Fatalf("preempt = %+v, code %q, %v", outcome, table.holdCode, err)
	}
	if cause := awaitCause(t, watch.Context()); !errors.Is(cause, domain.ErrFenceLost) {
		t.Fatalf("the running worker's cause = %v, want ErrFenceLost", cause)
	}
	if again, err := operator.Preempt(context.Background(), 7, "request-1"); err != nil || !again.Held() || again.Fence != 5 {
		t.Fatalf("preempting a held turn = %+v, %v", again, err)
	}
	cancelled := newControlTable(domain.TurnRunning, 1)
	reason := "stop"
	cancelled.reason = &reason
	if _, err := (&TableTurnControl{DB: cancelled}).Preempt(context.Background(), 7, "request-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a cancelled turn is not preempted: %v", err)
	}
	sql := turnControlQueries[qControlPreempt]
	for _, part := range []string{"fence = fence + 1", "hold_code = " + sqlHoldPreempted, "cancel_requested_at IS NULL", sqlTurnOwned, "partner_id = ? AND request_id = ?"} {
		if !strings.Contains(sql, part) {
			t.Errorf("preempt statement lacks %q", part)
		}
	}
}

// §4 gate: a release is refused while a call of the turn has an open outcome, in the statement
// that moves the turn, and allowed once the call settled.
func TestTableTurnControlReleaseIsRefusedWhileAnOutcomeIsOpen(t *testing.T) {
	table := newControlTable(domain.TurnHeld, 3)
	table.openOutcomes = 1
	control := &TableTurnControl{DB: table}
	if _, err := control.Release(context.Background(), 7, "request-1", 3); !errors.Is(err, domain.ErrConflict) || table.status != domain.TurnHeld {
		t.Fatalf("release with an open outcome = %v, status %s", err, table.status)
	}
	table.openOutcomes = 0
	if fence, err := control.Release(context.Background(), 7, "request-1", 3); err != nil || fence != 4 {
		t.Fatalf("release after resolution = %d, %v", fence, err)
	}
	sql := turnControlQueries[qControlRelease]
	if !strings.Contains(sql, "NOT EXISTS (SELECT 1 FROM turn_call_outcome o") || !strings.Contains(sql, "o.status_code IN ("+sqlCallOpen+")") ||
		!strings.Contains(sql, "o.partner_id = turn.partner_id AND o.request_id = turn.request_id") {
		t.Fatalf("the gate must be in the release's own statement: %s", sql)
	}
}

// §2: cancelling a turn marks its in-flight and accepted calls unknown in the cancelling transaction.
func TestTableTurnControlCancelMarksOpenCallsUnknown(t *testing.T) {
	table := newControlTable(domain.TurnHeld, 3)
	table.inFlight = 2
	if err := (&TableTurnControl{DB: table}).Cancel(context.Background(), 7, "request-1", "stop"); err != nil {
		t.Fatal(err)
	}
	if table.unknown != 2 || table.woken[0] != qControlUnknown {
		t.Fatalf("unknown %d, statements %v", table.unknown, table.woken)
	}
	if sql := turnControlQueries[qControlUnknown]; !strings.Contains(sql, "status_code = "+sqlCallUnknown) ||
		!strings.Contains(sql, "status_code IN ("+sqlCallInFlight+", "+sqlCallAccepted+")") || !strings.Contains(sql, "partner_id = ? AND request_id = ?") {
		t.Fatalf("cancel marking = %s", sql)
	}
}

// A preempting hold needs OPERATE and PREEMPT; it holds a running turn at once and records turn_held.
func TestStudioTurnControlPreemptNeedsPreempt(t *testing.T) {
	ctx := context.Background()
	request := domain.TurnHoldRequest{AgentID: "writer-a", RequestID: "request-1", Mode: domain.HoldModePreempt, Reason: "runaway refunds"}
	operator := newTurnControlFixture(domain.OperationView, domain.OperationOperate)
	operator.control.status["request-1"], operator.control.fence["request-1"] = domain.TurnRunning, 6
	if _, err := operator.service.Hold(ctx, usageActor, request); !errors.Is(err, domain.ErrForbidden) || len(operator.control.holds) != 0 {
		t.Fatalf("preempt without PREEMPT = %v, holds %v", err, operator.control.holds)
	}
	admin := newTurnControlFixture(domain.OperationView, domain.OperationOperate, domain.OperationPreempt)
	admin.control.status["request-1"], admin.control.fence["request-1"] = domain.TurnRunning, 6
	outcome, err := admin.service.Hold(ctx, usageActor, request)
	if err != nil || !outcome.Held() || outcome.Fence != 7 || admin.control.holds[0] != "request-1/"+string(domain.HoldPreempted) {
		t.Fatalf("preempt = %+v, %v, holds %v", outcome, err, admin.control.holds)
	}
	if records := admin.audit.Request(7, "request-1"); len(records) != 1 || records[0].Category != observability.AuditCategoryTurnHeld {
		t.Fatalf("audit = %+v", records)
	}
	preemptOnly := newTurnControlFixture(domain.OperationView, domain.OperationPreempt)
	if _, err := preemptOnly.service.Hold(ctx, usageActor, request); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("preempt without OPERATE = %v", err)
	}
}

// §8.1: a worker preempted mid-step is fenced out of every write; it records nothing terminal,
// settles nothing, publishes no final frame, and leaves the turn held.
func TestTurnRuntimePreemptedWorkerWritesNothing(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	fences := &MemoryTurnFences{}
	records := runtime.Records.(*fake.TurnRecordStore)
	records.AcquireFunc = func(context.Context, domain.TurnClaim) (domain.TurnOwner, error) {
		return fences.Acquire(7, "request-1")
	}
	failFunc := records.FailFunc
	records.FailFunc = func(ctx context.Context, owner domain.TurnOwner, status domain.TurnStatus, code string, settlement domain.TurnSettlement) error {
		if err := fences.Check(owner); err != nil {
			return err
		}
		return failFunc(ctx, owner, status, code, settlement)
	}
	sessions := runtime.Sessions.(*fake.SessionCoordinator)
	sessions.CheckpointFunc = func(_ context.Context, owner domain.TurnOwner, _ int64, _ domain.StepCheckpoint) error {
		return fences.Check(owner)
	}
	runtime.Idempotency = &fake.StepIdempotencyStore{
		CommitFunc: func(_ context.Context, owner domain.TurnOwner, _ domain.ExecutionStep, _ domain.StepResult) error {
			return fences.Check(owner)
		},
		AbandonFunc: func(_ context.Context, owner domain.TurnOwner, _ domain.ExecutionStep) error {
			return fences.Check(owner)
		},
	}
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
			// The operator preempts while the step runs.
			fences.Hold(7, "request-1")
			return stepExecutor(recorder).Execute(ctx, input)
		}), nil
	})
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("the preempted worker = %v, want ErrFenceLost", err)
	}
	if len(recorder.failures) != 0 || recorder.committed != 0 || recorder.released != 0 || len(terminalAudits(recorder.audits)) != 0 || len(finalFrames(recorder.frames)) != 0 {
		t.Fatalf("a preempted worker wrote: failures %v, settled %d, refunded %d, audits %v, frames %+v",
			recorder.failures, recorder.committed, recorder.released, recorder.audits, recorder.frames)
	}
}
