package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

type runtimeRecorder struct {
	calls     []string
	frames    []domain.TurnReply
	executed  []string
	committed int
	released  int
	usage     []domain.Usage
	failures  []string
	audits    []string
	snapshot  domain.SessionSnapshot
}

func (recorder *runtimeRecorder) record(call string) { recorder.calls = append(recorder.calls, call) }

func runtimeGraph() domain.ExecutionGraph {
	return domain.ExecutionGraph{
		AgentID: "agent", Version: "v1", EntryStepID: "step-1",
		Steps: []domain.ExecutionStep{
			{ExecutionStepID: 1, StepID: "step-1", Kind: "plan"},
			{ExecutionStepID: 2, StepID: "step-2", Kind: "answer"},
		},
	}
}

func newTestRuntime(t *testing.T, recorder *runtimeRecorder) *TurnRuntime {
	t.Helper()
	recorder.snapshot = domain.SessionSnapshot{ConversationID: "conversation-1", AgentVersion: "v1", Revision: 1}
	return &TurnRuntime{
		Records: &fake.TurnRecordStore{
			StartFunc: func(context.Context, int64, string) error { recorder.record("start"); return nil },
			FailFunc: func(_ context.Context, _ int64, _ string, status, errorCode string) error {
				recorder.record("fail")
				recorder.failures = append(recorder.failures, status+":"+errorCode)
				return nil
			},
			RecordUsageFunc: func(_ context.Context, _ int64, _ string, _ int64, _ string, _ domain.UsageAttribution, usage domain.Usage) error {
				recorder.record("usage")
				recorder.usage = append(recorder.usage, usage)
				return nil
			},
		},
		Sessions: &fake.SessionCoordinator{
			LoadFunc: func(context.Context, int64, string) (domain.SessionSnapshot, error) {
				recorder.record("load")
				return recorder.snapshot, nil
			},
			CheckpointFunc: func(context.Context, int64, int64, domain.StepCheckpoint) error {
				recorder.record("checkpoint")
				return nil
			},
			CompleteFunc: func(context.Context, int64, string, int64, domain.TurnResult) error {
				recorder.record("complete")
				return nil
			},
		},
		Definitions: fake.DefinitionResolverFunc(func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
			return runtimeGraph(), nil
		}),
		Policies: fake.TenantPolicyRepositoryFunc(func(context.Context, int64) (domain.TenantRuntimePolicy, error) {
			return domain.TenantRuntimePolicy{MaxSteps: 10, MaxTokens: 1000, CostCurrency: "USD", TurnTimeout: time.Minute}, nil
		}),
		Governor: &fake.ExecutionGovernor{},
		Executors: fake.StepExecutorRegistryFunc(func(_ context.Context, kind string) (contract.StepExecutor, error) {
			return stepExecutor(recorder), nil
		}),
		Idempotency: &fake.StepIdempotencyStore{},
		Guardrails:  &fake.GuardrailEnforcer{},
		Publisher: fake.TurnReplyPublisherFunc(func(_ context.Context, reply domain.TurnReply) error {
			recorder.record("publish")
			recorder.frames = append(recorder.frames, reply)
			return nil
		}),
		Estimator: &fake.TurnBudgetEstimator{},
		Budget: &fake.TenantBudgetManager{
			ReserveFunc: func(_ context.Context, tenantID int64, requestID string, tokens, cost int64, currency string) (domain.BudgetReservation, error) {
				return domain.BudgetReservation{TenantID: tenantID, ReservationID: "reservation-1", RequestID: requestID, Attempt: 1, GrantedTokens: tokens, Currency: currency}, nil
			},
			CommitFunc: func(context.Context, domain.BudgetReservation, domain.Usage) error {
				recorder.record("settle")
				recorder.committed++
				return nil
			},
			ReleaseFunc: func(context.Context, domain.BudgetReservation) error {
				recorder.record("refund")
				recorder.released++
				return nil
			},
		},
		Audit: &fake.AuditSink{RecordFunc: func(_ context.Context, event domain.DecisionRecord) error {
			recorder.audits = append(recorder.audits, event.Category)
			return nil
		}},
		MaxSteps: 8,
		Now:      func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
}

func stepExecutor(recorder *runtimeRecorder) contract.StepExecutor {
	return fake.StepExecutorFunc(func(_ context.Context, input domain.StepInput) (domain.StepResult, error) {
		recorder.record("execute")
		recorder.executed = append(recorder.executed, input.Step.StepID)
		next := "step-2"
		if input.Step.StepID == "step-2" {
			next = ""
		}
		return domain.StepResult{
			State: []byte(input.Step.StepID + "-state"), NextStepID: next,
			Usage: domain.Usage{OutputTokens: 5, CostMinorUnits: 2, Currency: "USD"},
		}, nil
	})
}

func runtimeDispatch() domain.TurnDispatch {
	return domain.TurnDispatch{
		Turn: domain.TurnRequest{
			TenantContext:  domain.TenantContext{TenantID: 7},
			RequestID:      "request-1",
			ConversationID: "conversation-1",
			AgentID:        "agent",
			Input:          []byte("hello"),
		},
		ReplyRoute: "route:request-1",
		EnqueuedAt: time.Unix(1_699_999_999, 0).UTC(),
	}
}

func callIndex(calls []string, name string) int {
	for i, call := range calls {
		if call == name {
			return i
		}
	}
	return -1
}

func countCalls(calls []string, name string) int {
	count := 0
	for _, call := range calls {
		if call == name {
			count++
		}
	}
	return count
}

func TestTurnRuntimeRunsGraphAndSettlesOnce(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	result, err := runtime.HandleTurn(context.Background(), runtimeDispatch())
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Response) != "step-2-state" || result.AgentVersion != "v1" {
		t.Fatalf("result = %+v", result)
	}
	if result.Usage.OutputTokens != 10 || result.Usage.CostMinorUnits != 4 || result.Usage.Currency != "USD" {
		t.Fatalf("usage = %+v", result.Usage)
	}
	if recorder.committed != 1 || recorder.released != 0 {
		t.Fatalf("settled %d times, refunded %d", recorder.committed, recorder.released)
	}
	if len(recorder.frames) != 3 || recorder.frames[0].Sequence != 0 || recorder.frames[1].Sequence != 1 ||
		!recorder.frames[2].Final || recorder.frames[2].Sequence != 2 {
		t.Fatalf("frames = %+v", recorder.frames)
	}
	if string(recorder.frames[0].Payload) != "step-1-state" {
		t.Fatalf("first frame payload = %q", recorder.frames[0].Payload)
	}
	// checkpoint precedes publish; the terminal record and settlement precede the final frame.
	if callIndex(recorder.calls, "checkpoint") > callIndex(recorder.calls, "publish") {
		t.Fatalf("calls = %v", recorder.calls)
	}
	last := len(recorder.calls) - 1
	if recorder.calls[last] != "publish" || callIndex(recorder.calls, "complete") > last ||
		callIndex(recorder.calls, "settle") > callIndex(recorder.calls, "complete") ||
		callIndex(recorder.calls, "usage") > callIndex(recorder.calls, "complete") {
		t.Fatalf("calls = %v", recorder.calls)
	}
	if len(recorder.usage) != 1 || recorder.usage[0].CostMinorUnits != 4 {
		t.Fatalf("usage events = %+v", recorder.usage)
	}
	if len(recorder.audits) != 1 || recorder.audits[0] != "turn_completed" {
		t.Fatalf("audits = %v", recorder.audits)
	}
}

func TestTurnRuntimeReplaysStoredStepResultInsteadOfSideEffect(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	// A worker crashed after committing step 1 and checkpointing it.
	recorder.snapshot = domain.SessionSnapshot{ConversationID: "conversation-1", AgentVersion: "v1", Revision: 2, LatestTurnNo: 1, LatestStepNo: 1}
	runtime.Idempotency = &fake.StepIdempotencyStore{BeginFunc: func(_ context.Context, _ int64, _ string, step domain.ExecutionStep) (domain.StepResult, bool, error) {
		if step.StepID == "step-1" {
			return domain.StepResult{State: []byte("step-1-state"), NextStepID: "step-2", Usage: domain.Usage{OutputTokens: 5, CostMinorUnits: 2, Currency: "USD"}}, true, nil
		}
		return domain.StepResult{}, false, nil
	}}
	if _, err := runtime.HandleTurn(context.Background(), runtimeDispatch()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.executed) != 1 || recorder.executed[0] != "step-2" {
		t.Fatalf("executed = %v, want only the unfinished step", recorder.executed)
	}
	// The checkpointed step is not rewritten, but its frame is republished at the same sequence.
	if countCalls(recorder.calls, "checkpoint") != 1 {
		t.Fatalf("calls = %v", recorder.calls)
	}
	if len(recorder.frames) != 3 || recorder.frames[0].Sequence != 0 || string(recorder.frames[0].Payload) != "step-1-state" {
		t.Fatalf("frames = %+v", recorder.frames)
	}
}

func TestTurnRuntimeDuplicateDeliveryOnlyReplaysTheFinalFrame(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	recorder.snapshot = domain.SessionSnapshot{ConversationID: "conversation-1", AgentVersion: "v1", Revision: 3, LatestTurnNo: 1, LatestStepNo: 2}
	runtime.Records = &fake.TurnRecordStore{FindFunc: func(context.Context, int64, string) (int64, string, []byte, error) {
		return 1, "completed", []byte("step-2-state"), nil
	}}
	result, err := runtime.HandleTurn(context.Background(), runtimeDispatch())
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Response) != "step-2-state" {
		t.Fatalf("result = %+v", result)
	}
	if len(recorder.executed) != 0 || recorder.committed != 0 || recorder.released != 0 {
		t.Fatalf("executed = %v, settled = %d, refunded = %d", recorder.executed, recorder.committed, recorder.released)
	}
	if len(recorder.frames) != 1 || !recorder.frames[0].Final || recorder.frames[0].Sequence != 2 {
		t.Fatalf("frames = %+v", recorder.frames)
	}
}

func TestTurnRuntimeGuardrailRejectionPublishesNoRawOutput(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	rejection := errors.New("policy violation")
	runtime.Guardrails = &fake.GuardrailEnforcer{AfterModelChunkFunc: func(context.Context, domain.GuardrailConfig, domain.GuardrailSubject, domain.ModelChunk) (domain.ModelChunk, error) {
		return domain.ModelChunk{}, rejection
	}}
	_, err := runtime.HandleTurn(context.Background(), runtimeDispatch())
	if !errors.Is(err, rejection) {
		t.Fatalf("error = %v", err)
	}
	if countCalls(recorder.calls, "checkpoint") != 0 {
		t.Fatalf("rejected step was checkpointed: %v", recorder.calls)
	}
	if len(recorder.frames) != 1 || !recorder.frames[0].Final || len(recorder.frames[0].Payload) != 0 {
		t.Fatalf("frames = %+v", recorder.frames)
	}
	if recorder.released != 0 || recorder.committed != 1 {
		t.Fatalf("refunded %d times, settled %d", recorder.released, recorder.committed)
	}
	if len(recorder.usage) != 1 || recorder.usage[0].OutputTokens != 5 {
		t.Fatalf("usage = %+v", recorder.usage)
	}
	if len(recorder.failures) != 1 || recorder.failures[0] != "failed:internal" {
		t.Fatalf("failures = %v", recorder.failures)
	}
	if len(recorder.audits) != 1 || recorder.audits[0] != "turn_failed" {
		t.Fatalf("audits = %v", recorder.audits)
	}
}

func TestTurnRuntimeCancellationRefundsAndMarksCancelled(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(context.Context, domain.StepInput) (domain.StepResult, error) {
			return domain.StepResult{}, domain.ErrTurnCanceled
		}), nil
	})
	if _, err := runtime.HandleTurn(context.Background(), runtimeDispatch()); !errors.Is(err, domain.ErrTurnCanceled) {
		t.Fatalf("error = %v", err)
	}
	if recorder.released != 1 || recorder.committed != 0 {
		t.Fatalf("refunded %d times, settled %d", recorder.released, recorder.committed)
	}
	if len(recorder.failures) != 1 || recorder.failures[0] != "cancelled:canceled" {
		t.Fatalf("failures = %v", recorder.failures)
	}
}

func TestTurnRuntimeAbandonRefundsExhaustedDelivery(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	if err := runtime.Abandon(context.Background(), runtimeDispatch(), "retry budget exhausted"); err != nil {
		t.Fatal(err)
	}
	if recorder.released != 1 || recorder.committed != 0 {
		t.Fatalf("refunded %d times, settled %d", recorder.released, recorder.committed)
	}
	if len(recorder.failures) != 1 || recorder.failures[0] != "failed:execution_limit" {
		t.Fatalf("failures = %v", recorder.failures)
	}
	if len(recorder.frames) != 1 || !recorder.frames[0].Final {
		t.Fatalf("frames = %+v", recorder.frames)
	}
}

func TestTurnRuntimeSkipsSettlementWhenAlreadySettled(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Budget = &fake.TenantBudgetManager{
		ReserveFunc: func(context.Context, int64, string, int64, int64, string) (domain.BudgetReservation, error) {
			return domain.BudgetReservation{}, domain.ErrBudgetSettled
		},
		CommitFunc:  func(context.Context, domain.BudgetReservation, domain.Usage) error { recorder.committed++; return nil },
		ReleaseFunc: func(context.Context, domain.BudgetReservation) error { recorder.released++; return nil },
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeDispatch()); err != nil {
		t.Fatal(err)
	}
	if recorder.committed != 0 || recorder.released != 0 {
		t.Fatalf("settled %d times, refunded %d", recorder.committed, recorder.released)
	}
	if len(recorder.usage) != 1 {
		t.Fatalf("usage events = %+v", recorder.usage)
	}
}

func TestTurnRuntimeRejectsUnclassifiedBudgetConflict(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Budget = &fake.TenantBudgetManager{
		ReserveFunc: func(context.Context, int64, string, int64, int64, string) (domain.BudgetReservation, error) {
			return domain.BudgetReservation{}, domain.ErrConflict
		},
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeDispatch()); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if len(recorder.executed) != 0 {
		t.Fatalf("executed = %v", recorder.executed)
	}
}

func TestTurnRuntimeSettlesUsageWhenLaterStepFails(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	runtime.Executors = fake.StepExecutorRegistryFunc(func(_ context.Context, kind string) (contract.StepExecutor, error) {
		if kind == "answer" {
			return fake.StepExecutorFunc(func(context.Context, domain.StepInput) (domain.StepResult, error) {
				return domain.StepResult{}, errors.New("provider failed")
			}), nil
		}
		return stepExecutor(recorder), nil
	})
	if _, err := runtime.HandleTurn(context.Background(), runtimeDispatch()); err == nil {
		t.Fatal("expected turn failure")
	}
	if recorder.committed != 1 || recorder.released != 0 {
		t.Fatalf("settled %d times, refunded %d", recorder.committed, recorder.released)
	}
	if len(recorder.usage) != 1 || recorder.usage[0].OutputTokens != 5 {
		t.Fatalf("usage = %+v", recorder.usage)
	}
}

func TestTurnRuntimeRequiresCollaboratorsAndValidDispatch(t *testing.T) {
	empty := &TurnRuntime{}
	if _, err := empty.HandleTurn(context.Background(), runtimeDispatch()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
	runtime := newTestRuntime(t, &runtimeRecorder{})
	dispatch := runtimeDispatch()
	dispatch.ReplyRoute = ""
	if _, err := runtime.HandleTurn(context.Background(), dispatch); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}
