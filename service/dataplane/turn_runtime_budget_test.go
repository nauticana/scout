package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func perTurnBudget(tokens, cost int64, currency string) domain.ReleaseBudget {
	return domain.ReleaseBudget{Tokens: tokens, CostMinorUnits: cost, Currency: currency, Source: domain.BudgetSourceRelease,
		Period: domain.BudgetPeriod{Kind: domain.BudgetPerTurn}}
}

// budgetedRuntime runs the two-step test graph under a release budget, recording the policy the
// governor started with and the spend each executed step received.
func budgetedRuntime(t *testing.T, recorder *runtimeRecorder, budget domain.ReleaseBudget) (*TurnRuntime, *domain.TenantRuntimePolicy, *[]domain.StepSpend) {
	t.Helper()
	runtime := newTestRuntime(t, recorder)
	runtime.Policies = fake.TenantPolicyRepositoryFunc(func(context.Context, int64) (domain.TenantRuntimePolicy, error) {
		return domain.TenantRuntimePolicy{MaxSteps: 10, MaxTokens: 1000, MaxCostMinorUnits: 100, CostCurrency: "USD", TurnTimeout: time.Minute}, nil
	})
	started, spends := &domain.TenantRuntimePolicy{}, &[]domain.StepSpend{}
	runtime.Limits = &fake.PrincipalLimits{BudgetFunc: func(_ context.Context, principal domain.Principal) (domain.ReleaseBudget, error) {
		if principal.Release != "v1" || principal.ID != "agent" {
			return domain.ReleaseBudget{}, errors.New("budget read for the wrong release")
		}
		return budget, nil
	}}
	runtime.Governor = &fake.ExecutionGovernor{StartFunc: func(_ context.Context, _ domain.TurnRequest, policy domain.TenantRuntimePolicy) (contract.ExecutionPermit, error) {
		*started = policy
		return &fake.ExecutionPermit{}, nil
	}}
	runtime.Executors = fake.StepExecutorRegistryFunc(func(context.Context, string) (contract.StepExecutor, error) {
		return fake.StepExecutorFunc(func(ctx context.Context, input domain.StepInput) (domain.StepResult, error) {
			*spends = append(*spends, input.Spend)
			return stepExecutor(recorder).Execute(ctx, input)
		}), nil
	})
	return runtime, started, spends
}

func TestTurnRuntimeNarrowsTheGovernorByAPerTurnBudget(t *testing.T) {
	runtime, started, spends := budgetedRuntime(t, &runtimeRecorder{}, perTurnBudget(400, 30, "USD"))
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("HandleTurn: %v", err)
	}
	if started.MaxTokens != 400 || started.MaxCostMinorUnits != 30 {
		t.Fatalf("governor started with %+v", *started)
	}
	// Each step may spend only what the turn's budget has left.
	want := []domain.StepSpend{
		{Tokens: 400, CostMinorUnits: 30, Currency: "USD", PricingRequired: true},
		{Tokens: 395, CostMinorUnits: 28, Currency: "USD", PricingRequired: true},
	}
	if len(*spends) != 2 || (*spends)[0] != want[0] || (*spends)[1] != want[1] {
		t.Fatalf("spends = %+v", *spends)
	}
	runtime, started, _ = budgetedRuntime(t, &runtimeRecorder{}, perTurnBudget(5_000, 0, ""))
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("HandleTurn: %v", err)
	}
	if started.MaxTokens != 1000 || started.MaxCostMinorUnits != 100 {
		t.Fatalf("a looser per-turn budget widened the governor: %+v", *started)
	}
}

func TestTurnRuntimeRefusesAPerTurnBudgetInAnotherCurrency(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime, started, spends := budgetedRuntime(t, recorder, perTurnBudget(0, 30, "EUR"))
	_, err := runtime.HandleTurn(context.Background(), runtimeClaim())
	var unenforceable *domain.BudgetUnenforceableError
	if !errors.As(err, &unenforceable) || unenforceable.Problem != domain.BudgetCurrencyMismatch {
		t.Fatalf("error = %v, want a currency mismatch", err)
	}
	if started.MaxTokens != 0 || len(*spends) != 0 {
		t.Fatalf("the governor started (%+v) or a step ran (%v)", *started, *spends)
	}
}

func TestTurnRuntimeStopsAStepOnceThePerTurnBudgetIsSpent(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime, _, spends := budgetedRuntime(t, recorder, perTurnBudget(5, 0, ""))
	_, err := runtime.HandleTurn(context.Background(), runtimeClaim())
	if !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want the spent budget to stop step 2", err)
	}
	if len(*spends) != 1 || len(recorder.executed) != 1 || recorder.executed[0] != "step-1" {
		t.Fatalf("executed %v with %+v", recorder.executed, *spends)
	}
	// A step already committed replays even though the budget is now spent.
	replay := &runtimeRecorder{}
	runtime, _, spends = budgetedRuntime(t, replay, perTurnBudget(5, 0, ""))
	runtime.Idempotency = &fake.StepIdempotencyStore{BeginFunc: func(_ context.Context, _ domain.TurnOwner, step domain.ExecutionStep) (domain.StepResult, bool, error) {
		if step.StepID == "step-2" {
			return domain.StepResult{State: []byte("step-2-state"), Usage: domain.Usage{OutputTokens: 5, Currency: "USD"}}, true, nil
		}
		return domain.StepResult{}, false, nil
	}}
	if result, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil || string(result.Response) != "step-2-state" {
		t.Fatalf("replayed turn = %q, %v", result.Response, err)
	}
	if len(*spends) != 1 {
		t.Fatalf("a replayed step executed: %+v", *spends)
	}
}

func TestStepSpendRequiresPricingForAnyCostCeiling(t *testing.T) {
	monthly := domain.ReleaseBudget{CostMinorUnits: 5_000, Currency: "USD", Source: domain.BudgetSourceRelease,
		Period: domain.BudgetPeriod{Kind: domain.BudgetMonthly, Timezone: "UTC"}}
	execution := &turnExecution{budget: monthly, usage: domain.Usage{CostMinorUnits: 9_999}}
	spend, err := stepSpend(execution)
	if err != nil || spend != (domain.StepSpend{Currency: "USD", PricingRequired: true}) {
		t.Fatalf("monthly spend = %+v, %v; a period budget caps no single step", spend, err)
	}
	execution.budget = perTurnBudget(0, 10, "USD")
	execution.usage = domain.Usage{CostMinorUnits: 10}
	if _, err = stepSpend(execution); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("spent cost = %v", err)
	}
	if spend, err = stepSpend(&turnExecution{}); err != nil || spend != (domain.StepSpend{}) {
		t.Fatalf("unbudgeted = %+v, %v", spend, err)
	}
}

func TestTurnBudgetRequestIsGoverned(t *testing.T) {
	request := turnBudgetRequest(runtimeDispatch().Turn, domain.Usage{InputTokens: 10, OutputTokens: 5, CostMinorUnits: 3, Currency: "USD"})
	if !request.TurnGoverned || request.Tokens != 15 || request.Principal != (domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "agent"}) {
		t.Fatalf("request = %+v", request)
	}
}
