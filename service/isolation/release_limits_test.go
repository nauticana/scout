package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

type stubReleases domain.EffectiveRelease

func (s stubReleases) Get(context.Context, int64, string, string) (domain.EffectiveRelease, error) {
	return domain.EffectiveRelease(s), nil
}

func (stubReleases) Put(context.Context, domain.EffectiveRelease) error { return nil }

func releaseWith(kind domain.ResourceKind, value string) stubReleases {
	return stubReleases{Resources: []domain.EffectiveResource{{ResourceKind: kind, Value: []byte(value)}}}
}

func boundAgent() domain.Principal {
	return domain.Principal{Kind: domain.PrincipalAgent, ID: "agent", TenantID: 7, Release: "3"}
}

func TestBudgetComesFromTheFrozenRelease(t *testing.T) {
	limits := &ReleaseLimits{
		Releases: releaseWith(domain.ResourceBudget, `{"tokens":400,"cost_minor_units":5000,"currency":"EUR","exponent":2,"period":"month","timezone":"Europe/Istanbul"}`),
	}
	budget, err := limits.Budget(context.Background(), boundAgent())
	if err != nil {
		t.Fatal(err)
	}
	if budget.Tokens != 400 || budget.CostMinorUnits != 5000 || budget.Currency != "EUR" || budget.Exponent == nil || *budget.Exponent != 2 ||
		budget.Source != domain.BudgetSourceRelease || budget.Period != (domain.BudgetPeriod{Kind: domain.BudgetMonthly, Timezone: "Europe/Istanbul"}) {
		t.Fatalf("budget = %+v", budget)
	}
}

func TestReleaseBudgetWithoutAPeriodIsMalformed(t *testing.T) {
	for name, value := range map[string]string{
		"no period": `{"tokens":400,"cost_minor_units":5000,"currency":"EUR"}`,
		"malformed": `{"tokens":"many"}`,
	} {
		limits := &ReleaseLimits{Releases: releaseWith(domain.ResourceBudget, value)}
		if budget, err := limits.Budget(context.Background(), boundAgent()); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: budget = %+v, %v; want validation", name, budget, err)
		}
	}
}

func TestBudgetFallbackMustNameAValidPeriod(t *testing.T) {
	daily := domain.ReleaseBudget{Tokens: 10, Period: domain.BudgetPeriod{Kind: domain.BudgetDaily, Timezone: "UTC"}}
	limits := &ReleaseLimits{Releases: stubReleases{}, Fallback: daily}
	if budget, err := limits.Budget(context.Background(), boundAgent()); err != nil || budget.Source != domain.BudgetSourceFallback || budget.Tokens != 10 {
		t.Fatalf("fallback = %+v, %v", budget, err)
	}
	for name, fallback := range map[string]domain.ReleaseBudget{
		"no period":      {Tokens: 10},
		"invalid period": {Tokens: 10, Period: domain.BudgetPeriod{Kind: domain.BudgetDaily}},
	} {
		limits.Fallback = fallback
		if _, err := limits.Budget(context.Background(), boundAgent()); err == nil || errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want a configuration error", name, err)
		}
	}
	limits.Fallback = domain.ReleaseBudget{}
	if budget, err := limits.Budget(context.Background(), boundAgent()); err != nil || budget != (domain.ReleaseBudget{}) {
		t.Fatalf("unbounded fallback = %+v, %v", budget, err)
	}
}

func TestAutonomyDegradesOutsideTheOperatingWindow(t *testing.T) {
	limits := &ReleaseLimits{
		Releases: releaseWith(domain.ResourceAutonomy, `{"mode":"bounded_autonomous","window_from_minute":540,"window_to_minute":1020}`),
	}
	inside := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)
	outside := time.Date(2026, 8, 17, 22, 0, 0, 0, time.UTC)

	mode, err := limits.AutonomyMode(context.Background(), boundAgent(), inside)
	if err != nil || mode != domain.AutonomyBounded {
		t.Fatalf("inside window: mode = %q, error = %v", mode, err)
	}
	// Out of hours the agent asks instead of stopping, so work is not simply lost.
	mode, err = limits.AutonomyMode(context.Background(), boundAgent(), outside)
	if err != nil || mode != domain.AutonomyExecuteWithApproval {
		t.Fatalf("outside window: mode = %q, error = %v", mode, err)
	}
}

func TestAutonomyDefaultsToHumanOnlyWhenUnbound(t *testing.T) {
	limits := &ReleaseLimits{Releases: stubReleases{}}
	mode, err := limits.AutonomyMode(context.Background(), boundAgent(), time.Now())
	if err != nil || mode != domain.AutonomyHumanOnly {
		t.Fatalf("mode = %q, error = %v, want the closed default", mode, err)
	}
}

func TestLimitsRejectAnUnpinnedPrincipal(t *testing.T) {
	limits := &ReleaseLimits{Releases: stubReleases{}}
	unpinned := boundAgent()
	unpinned.Release = ""
	if _, err := limits.Budget(context.Background(), unpinned); err == nil {
		t.Fatal("an unpinned principal must not resolve limits")
	}
}
