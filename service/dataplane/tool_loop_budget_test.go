package dataplane

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestToolLoopStopsAtWhatTheTurnBudgetLeaves(t *testing.T) {
	distinct := func(id string) domain.ModelResult {
		return proposes(10, toolCall(id, "search", fmt.Sprintf(`{"q":%q}`, id)))
	}
	harness := newLoopHarness(t, distinct("a"), distinct("b"))
	input := loopInput()
	input.Spend = domain.StepSpend{Tokens: 30}
	if _, err := harness.executor().Execute(context.Background(), input); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want the turn's remaining tokens to stop the loop", err)
	}
	harness = newLoopHarness(t, distinct("a"))
	harness.invoke = func(domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte(`{}`), Usage: domain.Usage{CostMinorUnits: 9, Currency: "USD"}}, nil
	}
	input.Spend = domain.StepSpend{CostMinorUnits: 5, Currency: "USD", PricingRequired: true}
	if _, err := harness.executor().Execute(context.Background(), input); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("error = %v, want the turn's remaining cost to stop the loop", err)
	}
}

func TestToolLoopUnderACostBudgetNeedsPricedUsage(t *testing.T) {
	input := loopInput()
	input.Spend = domain.StepSpend{Currency: "USD", PricingRequired: true}

	unpriced := *newLoopHarness(t, answers("done")).executor()
	unpriced.Pricer = nil
	if _, err := unpriced.Execute(context.Background(), input); !errors.Is(err, domain.ErrDegraded) {
		t.Fatalf("no pricer = %v, want degraded", err)
	}

	missing := *newLoopHarness(t, answers("done")).executor()
	missing.Pricer = fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
		return 0, "", fmt.Errorf("%w: no price for model p/m", domain.ErrNotFound)
	})
	if _, err := missing.Execute(context.Background(), input); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unpriced model = %v, want the step to fail", err)
	}

	foreign := *newLoopHarness(t, answers("done")).executor()
	foreign.Pricer = fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
		return 3, "EUR", nil
	})
	if _, err := foreign.Execute(context.Background(), input); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("usage in another currency = %v, want validation", err)
	}

	priced := *newLoopHarness(t, answers("done")).executor()
	priced.Pricer = fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
		return 3, "USD", nil
	})
	result, err := priced.Execute(context.Background(), input)
	if err != nil || result.Usage.CostMinorUnits != 3 || result.Usage.Currency != "USD" {
		t.Fatalf("priced = %+v, %v", result.Usage, err)
	}
}
