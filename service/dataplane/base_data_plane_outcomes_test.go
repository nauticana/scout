package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/toolgateway"
)

// plainBudget is a budget manager that cannot join a turn's ending transaction.
type plainBudget struct{ contract.TenantBudgetManager }

// The composed plane records call outcomes in tables, settles turns through its budget in their
// ending transactions, and hands the loop the governed gateway as its reconciler with the dispatch
// window of the tool settings and the attestation grace; the composition's trusted declarations
// reach the gateway.
func TestBaseDataPlaneComposesCallOutcomes(t *testing.T) {
	plane := composablePlane()
	plane.Settings.OutcomeAttestGrace = 2 * time.Minute
	plane.ToolOutcomes = map[string]domain.ToolOutcomeTraits{"refund.issue": {DedupesByKey: true}}
	if err := plane.Compose(); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	t.Cleanup(func() { _ = plane.Close() })
	if _, ok := plane.Outcomes.(*TableCallOutcomeStore); !ok {
		t.Fatalf("outcomes = %T", plane.Outcomes)
	}
	gateway, ok := plane.Reconciler.(*toolgateway.GovernedGateway)
	if !ok || gateway != plane.ToolGateway || !gateway.Traits("refund.issue").DedupesByKey {
		t.Fatalf("reconciler = %T", plane.Reconciler)
	}
	loop, err := plane.Executors.ExecutorFor(context.Background(), domain.StepKindToolLoop)
	if err != nil {
		t.Fatal(err)
	}
	executor := loop.(*ToolLoopExecutor)
	if executor.Outcomes != plane.Outcomes || executor.Reconciler != plane.Reconciler ||
		executor.Dispatch != (domain.ToolDispatchWindow{Timeout: time.Second, Attempts: 2, Grace: 2 * time.Minute}) {
		t.Fatalf("loop outcomes %T, reconciler %T, dispatch %+v", executor.Outcomes, executor.Reconciler, executor.Dispatch)
	}
	if store := plane.Records.(*TableTurnRecordStore); any(store.Settlement.Budget) != any(plane.Budget) {
		t.Fatal("the turn record store must settle through the composed budget")
	}

	defaulted := composablePlane()
	if err := defaulted.Compose(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = defaulted.Close() })
	loop, _ = defaulted.Executors.ExecutorFor(context.Background(), domain.StepKindToolLoop)
	if grace := loop.(*ToolLoopExecutor).Dispatch.Grace; grace != DefaultOutcomeAttestGrace {
		t.Fatalf("a zero grace takes the default, got %s", grace)
	}

	plain := composablePlane()
	plain.Budget = plainBudget{}
	if err := plain.Compose(); !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "TurnBudgetSettler") {
		t.Fatalf("a budget that cannot settle in the turn's transaction = %v", err)
	}
	negative := composablePlane()
	negative.Settings.OutcomeAttestGrace = -time.Second
	if err := negative.Compose(); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative grace = %v", err)
	}
	undeclared := composablePlane()
	undeclared.ToolOutcomes = map[string]domain.ToolOutcomeTraits{"refund.issue": {Lookup: true}}
	if err := undeclared.Compose(); err == nil {
		t.Fatal("a lookup declaration without a lookup must refuse the composition")
	}
}
