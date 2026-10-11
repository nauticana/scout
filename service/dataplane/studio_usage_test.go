package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

type allowStudio struct{ action domain.StudioAction }

func (allow allowStudio) Authorize(_ context.Context, _ domain.StudioActor, action domain.StudioAction, _ string) error {
	if action != allow.action {
		return domain.ErrForbidden
	}
	return nil
}

var (
	usageActor = domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}
	usageFrom  = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
)

func TestStudioUsageReportsCostPerWorkItemWithItsExponent(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qStudioUsage: {
		{"4", "human", "42", "task-9", "JPY", int64(0), int64(2), int64(300), int64(80), int64(1), int64(120)},
	}}}
	usage := &StudioUsage{DB: queueDBFake{query: query}, Authorizer: allowStudio{domain.StudioView}}
	lines, err := usage.Usage(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(24*time.Hour))
	if err != nil || len(lines) != 1 {
		t.Fatalf("lines = %+v, error = %v", lines, err)
	}
	if line := lines[0]; line.WorkRef != "task-9" || line.OnBehalfOf.ID != "42" || line.Currency != "JPY" || line.Exponent != 0 || line.CostMinorUnits != 120 {
		t.Fatalf("line = %+v", line)
	}
	if args := query.firstArgs(qStudioUsage); args[0] != int64(7) || args[1] != "writer-a" || args[4] != MaxUsageLines+1 {
		t.Fatalf("args = %v", args)
	}
}

func TestStudioUsageRefusesUnboundedOrUngrantedReports(t *testing.T) {
	usage := &StudioUsage{DB: queueDBFake{query: &queueQueryFake{rows: map[string][][]any{}}}, Authorizer: allowStudio{domain.StudioView}}
	for name, to := range map[string]time.Time{"backwards": usageFrom.Add(-time.Hour), "too long": usageFrom.Add(MaxUsageWindow + time.Hour)} {
		if _, err := usage.Usage(context.Background(), usageActor, "writer-a", usageFrom, to); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v, want validation", name, err)
		}
	}
	usage.Authorizer = allowStudio{domain.StudioPublish}
	if _, err := usage.Usage(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(time.Hour)); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want forbidden", err)
	}
	rows := make([][]any, MaxUsageLines+1)
	for index := range rows {
		rows[index] = []any{"4", "", "", "", "USD", int64(2), int64(1), int64(1), int64(1), int64(0), int64(1)}
	}
	usage = &StudioUsage{DB: queueDBFake{query: &queueQueryFake{rows: map[string][][]any{qStudioUsage: rows}}}, Authorizer: allowStudio{domain.StudioView}}
	if _, err := usage.Usage(context.Background(), usageActor, "writer-a", usageFrom, usageFrom.Add(time.Hour)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversized report = %v, want validation", err)
	}
}
