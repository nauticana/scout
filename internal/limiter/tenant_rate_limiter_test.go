package limiter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestTenantRateLimiterEnforcesTenantAndFleet(t *testing.T) {
	now := time.Unix(0, 0)
	limiter := &TenantRateLimiter{
		Turn:       RateLimit{PerSecond: 1, Burst: 2},
		FleetTurn:  RateLimit{PerSecond: 1, Burst: 3},
		MaxTenants: 4096,
		Now:        func() time.Time { return now },
	}
	ctx := context.Background()
	tenant := domain.TenantContext{TenantID: 1}

	if err := limiter.AllowTurn(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := limiter.AllowTurn(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	err := limiter.AllowTurn(ctx, tenant)
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || !errors.Is(err, domain.ErrRateLimited) || limitErr.Scope != "turn.tenant" {
		t.Fatalf("tenant denial = %v", err)
	}
	if limitErr.RetryAfter() <= 0 {
		t.Fatalf("retry after = %s", limitErr.RetryAfter())
	}

	// A second tenant drains the remaining fleet burst.
	if err := limiter.AllowTurn(ctx, domain.TenantContext{TenantID: 2}); err != nil {
		t.Fatal(err)
	}
	err = limiter.AllowTurn(ctx, domain.TenantContext{TenantID: 3})
	if !errors.As(err, &limitErr) || limitErr.Scope != "turn.fleet" {
		t.Fatalf("fleet denial = %v", err)
	}

	now = now.Add(2 * time.Second)
	if err := limiter.AllowTurn(ctx, tenant); err != nil {
		t.Fatalf("refill: %v", err)
	}
}

func TestTenantRateLimiterLanesAreIndependent(t *testing.T) {
	limiter := &TenantRateLimiter{Turn: RateLimit{PerSecond: 1, Burst: 1}, MaxTenants: 4096}
	ctx := context.Background()
	tenant := domain.TenantContext{TenantID: 7}

	if err := limiter.AllowTurn(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	if err := limiter.AllowTurn(ctx, tenant); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("turn = %v", err)
	}
	// Tool and model lanes are unconfigured and stay open.
	if err := limiter.AllowToolCall(ctx, domain.ToolCall{TenantContext: tenant}); err != nil {
		t.Fatal(err)
	}
	if err := limiter.AllowModelCall(ctx, domain.ModelRequest{TenantContext: tenant}); err != nil {
		t.Fatal(err)
	}
}

func TestTenantRateLimiterCapAndSweep(t *testing.T) {
	now := time.Unix(0, 0)
	limiter := &TenantRateLimiter{
		Turn:       RateLimit{PerSecond: 100, Burst: 1},
		MaxTenants: 2,
		Now:        func() time.Time { return now },
	}
	ctx := context.Background()
	for id := int64(1); id <= 2; id++ {
		if err := limiter.AllowTurn(ctx, domain.TenantContext{TenantID: id}); err != nil {
			t.Fatal(err)
		}
	}
	// Buckets are drained (burst 1), so the sweep cannot evict and the map refuses.
	if err := limiter.AllowTurn(ctx, domain.TenantContext{TenantID: 3}); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("capacity = %v", err)
	}
	// Once refilled, the sweep drops full buckets and admits the new tenant.
	now = now.Add(time.Second)
	if err := limiter.AllowTurn(ctx, domain.TenantContext{TenantID: 3}); err != nil {
		t.Fatal(err)
	}
}

func TestTenantRateLimiterValidation(t *testing.T) {
	limiter := &TenantRateLimiter{MaxTenants: 4096}
	if err := limiter.AllowTurn(context.Background(), domain.TenantContext{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.AllowTurn(canceled, domain.TenantContext{TenantID: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestTenantRateLimiterRejectsPartialConfiguration(t *testing.T) {
	limiter := &TenantRateLimiter{Turn: RateLimit{PerSecond: 1}, MaxTenants: 4096}
	if err := limiter.AllowTurn(context.Background(), domain.TenantContext{TenantID: 1}); err == nil {
		t.Fatal("partial rate configuration must fail")
	}
}
