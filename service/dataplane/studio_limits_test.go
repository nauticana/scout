package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func eurExponent() *int { value := 2; return &value }

var dayBudget = domain.ReleaseBudget{Tokens: 50_000, CostMinorUnits: 2_000, Currency: "EUR", Exponent: eurExponent(),
	Source: domain.BudgetSourceRelease, Period: domain.BudgetPeriod{Kind: domain.BudgetDaily, Timezone: "Europe/Berlin"}}

// studioLimits answers the stable release "4" with three turns in the last minute; read
// receives the principal whose frozen limits were read and spent the one whose spend was.
func studioLimits(query *queueQueryFake, limits domain.ExecutionLimits, budget domain.ReleaseBudget, spend func(domain.ReleaseBudget) (domain.BudgetSpend, error), read *domain.Principal, spent *domain.PrincipalRef) *StudioLimits {
	if _, set := query.rows[qStudioLimitsRelease]; !set {
		query.rows[qStudioLimitsRelease] = [][]any{{"4", int64(3)}}
	}
	return &StudioLimits{
		DB: queueDBFake{query: query}, Authorizer: allowStudio{domain.StudioView},
		Currencies: &fake.CurrencyCatalog{Exponents: map[string]int{"EUR": 2}},
		ReleaseLimits: &fake.PrincipalLimits{
			ExecutionLimitsFunc: func(_ context.Context, principal domain.Principal) (domain.ExecutionLimits, error) {
				*read = principal
				return limits, nil
			},
			BudgetFunc: func(context.Context, domain.Principal) (domain.ReleaseBudget, error) { return budget, nil },
		},
		Spend: &fake.BudgetSpend{SpendFunc: func(_ context.Context, tenantID int64, principal domain.PrincipalRef, asked domain.ReleaseBudget) (domain.BudgetSpend, error) {
			if spent != nil && tenantID == 7 {
				*spent = principal
			}
			return spend(asked)
		}},
	}
}

func noSpend(domain.ReleaseBudget) (domain.BudgetSpend, error) {
	return domain.BudgetSpend{}, errors.New("no spend expected")
}

func TestStudioLimitsReportsTheStableReleaseWithItsPeriodSpend(t *testing.T) {
	var read domain.Principal
	var spent domain.PrincipalRef
	limits := domain.ExecutionLimits{TurnsPerMinute: 30, MaxSteps: 6, MaxToolCalls: 4, MaxDuration: time.Minute}
	start := time.Date(2026, 10, 10, 22, 0, 0, 0, time.UTC)
	period := domain.BudgetSpend{PeriodStart: start, PeriodEnd: start.Add(24 * time.Hour), Tokens: 1200, CostMinorUnits: 450,
		HeldTokens: 300, HeldCostMinorUnits: 20, Currency: "EUR", Exponent: 2}
	query := &queueQueryFake{rows: map[string][][]any{}}
	report, err := studioLimits(query, limits, dayBudget, func(asked domain.ReleaseBudget) (domain.BudgetSpend, error) {
		if asked.Period != dayBudget.Period {
			return domain.BudgetSpend{}, errors.New("spend asked for another period")
		}
		return period, nil
	}, &read, &spent).Limits(context.Background(), usageActor, "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentID != "writer-a" || report.Release != "4" || report.Limits != limits || report.TurnsLastMinute != 3 ||
		report.Budget == nil || report.Budget.Period != dayBudget.Period || report.BudgetProblem != "" {
		t.Fatalf("report = %+v", report)
	}
	if read.Kind != domain.PrincipalAgent || read.ID != "writer-a" || read.TenantID != 7 || read.Release != "4" {
		t.Fatalf("limits read for %+v, want the stable release", read)
	}
	if spent != (domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer-a"}) || report.Spend == nil || *report.Spend != period {
		t.Fatalf("spend of %+v = %+v", spent, report.Spend)
	}
	if args := query.firstArgs(qStudioLimitsRelease); args[0] != int64(7) || args[1] != "writer-a" {
		t.Fatalf("release args = %v", args)
	}
}

func TestStudioLimitsReportsAPerTurnBudgetWithoutSpend(t *testing.T) {
	var read domain.Principal
	perTurn := domain.ReleaseBudget{Tokens: 4_000, CostMinorUnits: 30, Currency: "EUR", Exponent: eurExponent(),
		Source: domain.BudgetSourceRelease, Period: domain.BudgetPeriod{Kind: domain.BudgetPerTurn}}
	query := &queueQueryFake{rows: map[string][][]any{}}
	report, err := studioLimits(query, domain.ExecutionLimits{}, perTurn, noSpend, &read, nil).Limits(context.Background(), usageActor, "writer-a")
	if err != nil || report.Budget == nil || report.Budget.Tokens != 4_000 || report.Spend != nil || report.BudgetProblem != "" {
		t.Fatalf("report = %+v, error = %v", report, err)
	}
	drifted := studioLimits(query, domain.ExecutionLimits{}, perTurn, noSpend, &read, nil)
	drifted.Currencies = &fake.CurrencyCatalog{Exponents: map[string]int{"EUR": 3}}
	report, err = drifted.Limits(context.Background(), usageActor, "writer-a")
	if err != nil || report.BudgetProblem != domain.BudgetExponentChanged || report.Spend != nil {
		t.Fatalf("drifted per-turn report = %+v, error = %v", report, err)
	}
	report, err = studioLimits(query, domain.ExecutionLimits{}, domain.ReleaseBudget{}, noSpend, &read, nil).Limits(context.Background(), usageActor, "writer-a")
	if err != nil || report.Budget != nil || report.Spend != nil {
		t.Fatalf("unbudgeted report = %+v, error = %v", report, err)
	}
}

func TestStudioLimitsReportsAnUnenforceableBudgetInsteadOfFailing(t *testing.T) {
	var read domain.Principal
	invalidPeriod := domain.ReleaseBudget{Tokens: 100, Source: domain.BudgetSourceFallback, Period: domain.BudgetPeriod{Kind: domain.BudgetDaily, Timezone: "Mars/Olympus"}}
	for problem, budget := range map[domain.BudgetProblem]domain.ReleaseBudget{domain.BudgetInvalidPeriod: invalidPeriod, domain.BudgetExponentChanged: dayBudget} {
		refuse := func(domain.ReleaseBudget) (domain.BudgetSpend, error) {
			return domain.BudgetSpend{}, &domain.BudgetUnenforceableError{Problem: problem, Detail: "test"}
		}
		report, err := studioLimits(&queueQueryFake{rows: map[string][][]any{}}, domain.ExecutionLimits{}, budget, refuse, &read, nil).
			Limits(context.Background(), usageActor, "writer-a")
		if err != nil || report.BudgetProblem != problem || report.Budget == nil || report.Spend != nil {
			t.Errorf("%s: report = %+v, error = %v", problem, report, err)
		}
	}
	broken := func(domain.ReleaseBudget) (domain.BudgetSpend, error) {
		return domain.BudgetSpend{}, errors.New("database down")
	}
	if _, err := studioLimits(&queueQueryFake{rows: map[string][][]any{}}, domain.ExecutionLimits{}, dayBudget, broken, &read, nil).
		Limits(context.Background(), usageActor, "writer-a"); err == nil {
		t.Fatal("a failed spend read must fail the report")
	}
}

func TestStudioLimitsRefusesAnUnpublishedAgentAndAnUngrantedActor(t *testing.T) {
	var read domain.Principal
	query := &queueQueryFake{rows: map[string][][]any{qStudioLimitsRelease: nil}}
	limits := studioLimits(query, domain.ExecutionLimits{}, domain.ReleaseBudget{}, noSpend, &read, nil)
	if _, err := limits.Limits(context.Background(), usageActor, "writer-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
	query = &queueQueryFake{rows: map[string][][]any{}}
	limits = studioLimits(query, domain.ExecutionLimits{}, dayBudget, noSpend, &read, nil)
	limits.Authorizer = allowStudio{domain.StudioEdit}
	if _, err := limits.Limits(context.Background(), usageActor, "writer-a"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want forbidden", err)
	}
	if len(query.calls) != 0 || read.ID != "" {
		t.Fatalf("an ungranted actor read %v and the limits of %+v", query.calls, read)
	}
	if _, err := (&StudioLimits{DB: queueDBFake{query: query}, Authorizer: allowStudio{domain.StudioView}}).Limits(context.Background(), usageActor, "writer-a"); err == nil {
		t.Fatal("a report without release limits must be refused")
	}
	missingSpend := studioLimits(query, domain.ExecutionLimits{}, dayBudget, noSpend, &read, nil)
	missingSpend.Spend = nil
	if _, err := missingSpend.Limits(context.Background(), usageActor, "writer-a"); err == nil || len(query.calls) != 0 {
		t.Fatalf("a report without a spend reader = %v after %v", err, query.calls)
	}
}
