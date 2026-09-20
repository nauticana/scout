package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/observability"
)

// chainHarness drives one request from ingress through as many deliveries as a
// test asks for, into one audit sink.
type chainHarness struct {
	sink    *observability.MemoryAuditSink
	ingress *TurnIngress
	runtime *TurnRuntime
	status  string
}

func newChainHarness(t *testing.T) *chainHarness {
	t.Helper()
	harness := &chainHarness{sink: &observability.MemoryAuditSink{}}
	harness.ingress = newTestIngress(t, &ingressRecorder{}, nil)
	harness.ingress.Audit = harness.sink
	harness.runtime = newTestRuntime(t, &runtimeRecorder{})
	harness.runtime.Audit = harness.sink
	// The turn record is what makes a later delivery a terminal replay.
	harness.runtime.Records = &fake.TurnRecordStore{
		FindFunc: func(context.Context, int64, string) (int64, string, []byte, error) {
			return 1, harness.status, nil, nil
		},
		FailFunc: func(_ context.Context, _ int64, _ string, status, _ string) error {
			harness.status = status
			return nil
		},
	}
	return harness
}

// stepDecision stands for any governed decision a step makes: it records under the step's scope.
func (harness *chainHarness) stepDecision(failFirst *bool) contract.StepExecutorRegistry {
	return fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
			turn := runtimeDispatch().Turn
			err := harness.sink.Record(ctx, domain.DecisionRecord{
				TenantID: turn.TenantContext.TenantID, Principal: turnPrincipal(turn), Category: domain.DecisionCategoryModelRoute,
				Action: "route", Resource: "model-a", Outcome: domain.DecisionAllow, RequestID: turn.RequestID,
			})
			if err == nil && *failFirst {
				*failFirst, err = false, errors.New("worker lost after the decision was recorded")
			}
			return domain.StepResult{State: []byte("done")}, err
		}), nil
	})
}

func (harness *chainHarness) chain() []domain.DecisionRecord {
	turn := runtimeDispatch().Turn
	return harness.sink.Request(turn.TenantContext.TenantID, turn.RequestID)
}

func categories(records []domain.DecisionRecord) (names []string) {
	for _, record := range records {
		names = append(names, record.Category)
	}
	return names
}

func TestDecisionChainIsGapFreeAndRecordedOnceAcrossRedelivery(t *testing.T) {
	harness := newChainHarness(t)
	ctx := context.Background()
	if _, err := harness.ingress.OpenTurn(ctx, runtimeDispatch().Turn); err != nil {
		t.Fatalf("OpenTurn: %v", err)
	}
	// A retried admission of the same request is the same decision.
	if _, err := harness.ingress.OpenTurn(ctx, runtimeDispatch().Turn); err != nil {
		t.Fatalf("OpenTurn again: %v", err)
	}

	failFirst := true
	harness.runtime.Executors = harness.stepDecision(&failFirst)
	// First delivery: the step's decision is recorded, then the worker fails and the turn ends failed.
	if _, err := harness.runtime.HandleTurn(ctx, runtimeDispatch()); err == nil {
		t.Fatal("the first delivery must fail")
	}
	// The nacked delivery comes back twice; the turn is terminal, so each is a replay.
	for range 2 {
		_, _ = harness.runtime.HandleTurn(ctx, runtimeDispatch())
	}
	want := []string{observability.AuditCategoryTurnAdmitted, domain.DecisionCategoryModelRoute, "turn_failed"}
	if got := categories(harness.chain()); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	if err := observability.VerifyTurnChain(harness.chain()); err != nil {
		t.Fatalf("VerifyTurnChain: %v", err)
	}
}

// A terminal record whose write was lost is offered again by the terminal replay, which closes the gap.
func TestTerminalReplayClosesAGapLeftByALostAuditWrite(t *testing.T) {
	harness := newChainHarness(t)
	ctx := context.Background()
	if _, err := harness.ingress.OpenTurn(ctx, runtimeDispatch().Turn); err != nil {
		t.Fatal(err)
	}
	lost := true
	harness.runtime.Audit = &fake.AuditSink{RecordFunc: func(ctx context.Context, decision domain.DecisionRecord) error {
		if lost && decision.Category == "turn_failed" {
			lost = false
			return errors.New("audit store unavailable")
		}
		return harness.sink.Record(ctx, decision)
	}}
	harness.runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(context.Context, domain.StepInput) (domain.StepResult, error) {
			return domain.StepResult{}, errors.New("provider failed")
		}), nil
	})
	if _, err := harness.runtime.HandleTurn(ctx, runtimeDispatch()); err == nil {
		t.Fatal("the delivery must fail")
	}
	if err := observability.VerifyTurnChain(harness.chain()); err == nil {
		t.Fatal("a chain without its terminal state must not verify")
	}
	_, _ = harness.runtime.HandleTurn(ctx, runtimeDispatch())
	if err := observability.VerifyTurnChain(harness.chain()); err != nil {
		t.Fatalf("the replay must close the gap: %v", err)
	}
}

func TestVerifyTurnChainNamesTheGap(t *testing.T) {
	admitted := domain.DecisionRecord{TenantID: 7, RequestID: "r", Category: observability.AuditCategoryTurnAdmitted, DecisionKey: "a"}
	completed := domain.DecisionRecord{TenantID: 7, RequestID: "r", Category: "turn_completed", DecisionKey: "c"}
	unkeyed := domain.DecisionRecord{TenantID: 7, RequestID: "r", Category: domain.DecisionCategoryGuardrail}
	for name, records := range map[string][]domain.DecisionRecord{
		"empty":               nil,
		"no admission":        {completed},
		"no terminal state":   {admitted},
		"decision after end":  {admitted, completed, {TenantID: 7, RequestID: "r", Category: "guardrail", DecisionKey: "g"}},
		"repeated decision":   {admitted, admitted, completed},
		"unkeyed decision":    {admitted, unkeyed, completed},
		"two terminal states": {admitted, completed, {TenantID: 7, RequestID: "r", Category: "turn_failed", DecisionKey: "f"}},
	} {
		if err := observability.VerifyTurnChain(records); err == nil {
			t.Errorf("%s: expected a chain error", name)
		}
	}
	if err := observability.VerifyTurnChain([]domain.DecisionRecord{admitted, completed}); err != nil {
		t.Fatalf("a complete chain must verify: %v", err)
	}
}
