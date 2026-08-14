package isolation

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestNewTenantRateLimiterBuildsConfiguredLanes(t *testing.T) {
	rateLimiter, err := NewTenantRateLimiter(RateLimiterConfig{Turn: RateLimit{PerSecond: 1, Burst: 1}, MaxTenants: 4096})
	if err != nil {
		t.Fatal(err)
	}
	tenant := domain.TenantContext{TenantID: 1}
	if err = rateLimiter.AllowTurn(context.Background(), tenant); err != nil {
		t.Fatal(err)
	}
	if err = rateLimiter.AllowTurn(context.Background(), tenant); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("second turn = %v", err)
	}
}

func TestNewTenantRateLimiterRejectsPartialConfiguration(t *testing.T) {
	if _, err := NewTenantRateLimiter(RateLimiterConfig{Model: RateLimit{PerSecond: 1}, MaxTenants: 4096}); err == nil {
		t.Fatal("partial rate must fail")
	}
}
