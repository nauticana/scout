package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func newEntitlementPolicy(query *budgetQueryFake, now func() time.Time) *EntitlementBudgetPolicy {
	return &EntitlementBudgetPolicy{
		DB:       budgetDBFake{query: query},
		Resource: "AI_CREDITS", Currency: "CRD", UsageCategory: "workspace",
		Now: now,
	}
}

func TestEntitlementPolicyProjectsRemainingMonthlyBudget(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	query := &budgetQueryFake{rows: map[string][][]any{
		qEntitlementPlan:          {{int64(10_000), "M"}},
		qEntitlementExternalUsage: {{int64(1_250)}},
	}}
	policy := newEntitlementPolicy(query, func() time.Time { return now })

	limits, err := policy.BudgetFor(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if limits.WindowCostMinorUnits != 8_750 || limits.Currency != "CRD" || limits.WindowTokens <= 0 {
		t.Fatalf("limits = %+v", limits)
	}
	if limits.Window != 12*24*time.Hour+12*time.Hour {
		t.Fatalf("window = %s", limits.Window)
	}
	args := query.args[qEntitlementExternalUsage]
	wantStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if len(args) != 6 || args[0] != int64(7) || args[1] != "AI_CREDITS" || args[2] != wantStart ||
		args[3] != int64(7) || args[4] != "workspace" || args[5] != wantStart {
		t.Fatalf("external usage args = %v", args)
	}
}

func TestEntitlementPolicyFailsClosedWithoutEntitlement(t *testing.T) {
	query := &budgetQueryFake{rows: map[string][][]any{
		qEntitlementPlan: {{nil, nil}},
	}}
	policy := newEntitlementPolicy(query, nil)
	if _, err := policy.BudgetFor(context.Background(), 7); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("missing entitlement = %v", err)
	}
}

func TestEntitlementWindowRejectsUnboundedPeriod(t *testing.T) {
	if _, _, err := entitlementWindow("L", time.Now()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("lifetime window = %v", err)
	}
}
