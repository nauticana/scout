package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/limiter"
)

func TestWindowedCostBreakerTripsPerScope(t *testing.T) {
	now := time.Unix(0, 0)
	breaker := &WindowedCostBreaker{
		TenantLimit: 100, AgentLimit: 60, FleetLimit: 150,
		Currency: "USD", Window: time.Minute, MaxEntries: 4096, Now: func() time.Time { return now },
	}
	ctx := context.Background()
	usage := func(cost int64) domain.Usage {
		return domain.Usage{CostMinorUnits: cost, Currency: "USD"}
	}

	if err := breaker.Record(ctx, 1, "writer", usage(50)); err != nil {
		t.Fatal(err)
	}
	var limitErr *limiter.LimitError
	err := breaker.Allow(ctx, 1, "writer", 20)
	if !errors.As(err, &limitErr) || !errors.Is(err, domain.ErrCircuitOpen) || limitErr.Scope != "cost.agent" {
		t.Fatalf("agent trip = %v", err)
	}
	// A different agent of the same tenant is capped by the tenant scope.
	if err := breaker.Record(ctx, 1, "coder", usage(40)); err != nil {
		t.Fatal(err)
	}
	err = breaker.Allow(ctx, 1, "coder", 15)
	if !errors.As(err, &limitErr) || limitErr.Scope != "cost.tenant" {
		t.Fatalf("tenant trip = %v", err)
	}
	// Another tenant hits the fleet limit.
	if err := breaker.Record(ctx, 2, "writer", usage(55)); err != nil {
		t.Fatal(err)
	}
	err = breaker.Allow(ctx, 2, "writer", 10)
	if !errors.As(err, &limitErr) || limitErr.Scope != "cost.fleet" {
		t.Fatalf("fleet trip = %v", err)
	}

	// The window slides: old cost rotates out and work is admitted again.
	now = now.Add(2 * time.Minute)
	if err := breaker.Allow(ctx, 1, "writer", 20); err != nil {
		t.Fatalf("after window = %v", err)
	}
}

func TestWindowedCostBreakerValidation(t *testing.T) {
	breaker := &WindowedCostBreaker{}
	if err := breaker.Allow(context.Background(), 1, "a", 1); err == nil {
		t.Fatal("missing window must error")
	}
	configured := &WindowedCostBreaker{Window: time.Minute, MaxEntries: 4096}
	if err := configured.Allow(context.Background(), 0, "a", -1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
	if err := configured.Record(context.Background(), 1, "a", domain.Usage{}); err != nil {
		t.Fatalf("zero cost record = %v", err)
	}
}

func TestWindowedCostBreakerBoundsEntriesAndCurrency(t *testing.T) {
	breaker := &WindowedCostBreaker{TenantLimit: 100, Currency: "USD", Window: time.Minute, MaxEntries: 1}
	usage := domain.Usage{CostMinorUnits: 1, Currency: "USD"}
	if err := breaker.Record(context.Background(), 1, "", usage); err != nil {
		t.Fatal(err)
	}
	if err := breaker.Allow(context.Background(), 2, "", 1); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("admission capacity = %v", err)
	}
	if err := breaker.Record(context.Background(), 2, "", usage); err != nil {
		t.Fatalf("completed record = %v", err)
	}
	if records, cost := breaker.Untracked(); records != 1 || cost != 1 {
		t.Fatalf("untracked = %d/%d", records, cost)
	}
	if err := breaker.Record(context.Background(), 1, "", domain.Usage{CostMinorUnits: 1, Currency: "EUR"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("currency = %v", err)
	}
}

func TestWindowedCostBreakerReservesTrackingDuringAdmission(t *testing.T) {
	breaker := &WindowedCostBreaker{TenantLimit: 100, Currency: "USD", Window: time.Minute, MaxEntries: 1}
	if err := breaker.Allow(context.Background(), 1, "", 1); err != nil {
		t.Fatal(err)
	}
	if err := breaker.Allow(context.Background(), 2, "", 1); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("second admission = %v", err)
	}
}
