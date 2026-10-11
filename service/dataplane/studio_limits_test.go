package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var dayBudget = domain.BudgetLimits{WindowTokens: 50_000, WindowCostMinorUnits: 2_000, Currency: "EUR", Window: 24 * time.Hour}

// studioLimits answers the stable release "4" with three turns in the last minute; read
// receives the principal whose frozen limits were read.
func studioLimits(query *queueQueryFake, limits domain.ExecutionLimits, budget domain.BudgetLimits, now time.Time, read *domain.Principal) *StudioLimits {
	if _, set := query.rows[qStudioLimitsRelease]; !set {
		query.rows[qStudioLimitsRelease] = [][]any{{"4", int64(3)}}
	}
	return &StudioLimits{
		DB: queueDBFake{query: query}, Authorizer: allowStudio{domain.StudioView}, Now: func() time.Time { return now },
		ReleaseLimits: &fake.PrincipalLimits{
			ExecutionLimitsFunc: func(_ context.Context, principal domain.Principal) (domain.ExecutionLimits, error) {
				*read = principal
				return limits, nil
			},
			BudgetFunc: func(context.Context, domain.Principal) (domain.BudgetLimits, error) { return budget, nil },
		},
	}
}

func TestStudioLimitsReportsTheStableReleaseWithItsWindowSpend(t *testing.T) {
	var read domain.Principal
	limits := domain.ExecutionLimits{TurnsPerMinute: 30, MaxSteps: 6, MaxToolCalls: 4, MaxDuration: time.Minute}
	query := &queueQueryFake{rows: map[string][][]any{qStudioLimitsSpend: {{int64(1200), int64(450), int16(2)}}}}
	now := time.Date(2026, 10, 10, 22, 30, 0, 0, time.UTC)
	report, err := studioLimits(query, limits, dayBudget, now, &read).Limits(context.Background(), usageActor, "writer-a")
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentID != "writer-a" || report.Release != "4" || report.Limits != limits || report.TurnsLastMinute != 3 ||
		report.Budget == nil || *report.Budget != dayBudget {
		t.Fatalf("report = %+v", report)
	}
	if read.Kind != domain.PrincipalAgent || read.ID != "writer-a" || read.TenantID != 7 || read.Release != "4" {
		t.Fatalf("limits read for %+v, want the stable release", read)
	}
	start := now.Add(-24 * time.Hour)
	spend := report.Spend
	if spend == nil || !spend.PeriodStart.Equal(start) || !spend.PeriodEnd.Equal(now) || spend.Tokens != 1200 ||
		spend.CostMinorUnits != 450 || spend.Currency != "EUR" || spend.Exponent != 2 {
		t.Fatalf("spend = %+v", spend)
	}
	args := query.firstArgs(qStudioLimitsSpend)
	if len(args) != 6 || args[0] != "EUR" || args[1] != "EUR" || args[2] != int64(7) || args[3] != "writer-a" ||
		!args[4].(time.Time).Equal(start) || !args[5].(time.Time).Equal(now) {
		t.Fatalf("spend args = %v", args)
	}
	if args := query.firstArgs(qStudioLimitsRelease); args[0] != int64(7) || args[1] != "writer-a" {
		t.Fatalf("release args = %v", args)
	}
}

func TestStudioLimitsOmitsSpendWithoutAWindow(t *testing.T) {
	var read domain.Principal
	unwindowed := domain.BudgetLimits{WindowTokens: 4_000}
	query := &queueQueryFake{rows: map[string][][]any{}}
	report, err := studioLimits(query, domain.ExecutionLimits{}, unwindowed, time.Now(), &read).Limits(context.Background(), usageActor, "writer-a")
	if err != nil || report.Budget == nil || report.Budget.WindowTokens != 4_000 || report.Spend != nil {
		t.Fatalf("report = %+v, error = %v", report, err)
	}
	if query.firstArgs(qStudioLimitsSpend) != nil {
		t.Fatal("a budget without a window has nothing to sum")
	}
	report, err = studioLimits(query, domain.ExecutionLimits{}, domain.BudgetLimits{}, time.Now(), &read).Limits(context.Background(), usageActor, "writer-a")
	if err != nil || report.Budget != nil || report.Spend != nil {
		t.Fatalf("unbudgeted report = %+v, error = %v", report, err)
	}
}

func TestStudioLimitsRefusesAnUnpublishedAgentAndAnUngrantedActor(t *testing.T) {
	var read domain.Principal
	query := &queueQueryFake{rows: map[string][][]any{qStudioLimitsRelease: nil}}
	limits := studioLimits(query, domain.ExecutionLimits{}, domain.BudgetLimits{}, time.Now(), &read)
	if _, err := limits.Limits(context.Background(), usageActor, "writer-a"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
	query = &queueQueryFake{rows: map[string][][]any{}}
	limits = studioLimits(query, domain.ExecutionLimits{}, domain.BudgetLimits{}, time.Now(), &read)
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
}

func TestStudioLimitsRefusesABudgetInAnUnknownCurrency(t *testing.T) {
	var read domain.Principal
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	query := &queueQueryFake{rows: map[string][][]any{qStudioLimitsSpend: {{int64(10), int64(10), nil}}}}
	if _, err := studioLimits(query, domain.ExecutionLimits{}, dayBudget, now, &read).Limits(context.Background(), usageActor, "writer-a"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("unknown currency: error = %v, want validation", err)
	}
	tokensOnly := domain.BudgetLimits{WindowTokens: 100, Window: time.Hour}
	query = &queueQueryFake{rows: map[string][][]any{qStudioLimitsSpend: {{int64(10), int64(0), nil}}}}
	report, err := studioLimits(query, domain.ExecutionLimits{}, tokensOnly, now, &read).Limits(context.Background(), usageActor, "writer-a")
	if err != nil || report.Spend == nil || report.Spend.Tokens != 10 || report.Spend.Currency != "" {
		t.Fatalf("token-only spend = %+v, error = %v", report.Spend, err)
	}
}
