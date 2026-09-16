package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/keel/cache"
	keellimiter "github.com/nauticana/keel/limiter"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func distributedConfig() DistributedRateLimiterConfig {
	return DistributedRateLimiterConfig{
		Turn:               WindowLimit{Limit: 4, Window: time.Second},
		FleetTurn:          WindowLimit{Limit: 6, Window: time.Second},
		KeyPrefix:          "scout:rl",
		StoreTimeout:       50 * time.Millisecond,
		FallbackFraction:   0.5,
		FallbackMaxTenants: 64,
		RecoveryProbe:      time.Second,
	}
}

// Each Allow* must reach its own keel lane, so one lane's exhaustion never blocks another.
func TestTenantRateLimiterLanesAreIndependent(t *testing.T) {
	rl, err := NewTenantRateLimiter(RateLimiterConfig{Turn: RateLimit{PerSecond: 1, Burst: 1}, MaxTenants: 64})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tenant := domain.TenantContext{TenantID: 7}

	if err := rl.AllowTurn(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	err = rl.AllowTurn(ctx, tenant)
	var limit *keellimiter.LimitError
	if !errors.As(err, &limit) || limit.Scope != laneTurn+".partner" {
		t.Fatalf("turn denial = %v", err)
	}
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("turn denial lost Scout's rate-limit contract: %v", err)
	}
	// Unconfigured lanes stay open.
	if err := rl.AllowToolCall(ctx, domain.ToolCall{TenantContext: tenant}); err != nil {
		t.Fatal(err)
	}
	if err := rl.AllowModelCall(ctx, domain.ModelRequest{TenantContext: tenant}); err != nil {
		t.Fatal(err)
	}
	if err := rl.AllowTurn(ctx, domain.TenantContext{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing tenant = %v", err)
	}
}

func TestDistributedTenantRateLimiterSharesQuotaAndDegrades(t *testing.T) {
	store := &fake.CacheService{}
	ctx := context.Background()
	replicas := make([]*DistributedTenantRateLimiter, 2)
	for i := range replicas {
		var err error
		if replicas[i], err = NewDistributedTenantRateLimiter(store, nil, distributedConfig()); err != nil {
			t.Fatal(err)
		}
	}
	// Both replicas charge the same shared window: four admissions for the tenant, not four each.
	admitted := 0
	for i := range 6 {
		if err := replicas[i%2].AllowTurn(ctx, domain.TenantContext{TenantID: 1}); err == nil {
			admitted++
		}
	}
	if admitted != 4 {
		t.Fatalf("admitted %d across replicas, want the shared tenant limit", admitted)
	}
	if err := replicas[0].AllowTurn(ctx, domain.TenantContext{TenantID: 1}); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("shared denial lost Scout's rate-limit contract: %v", err)
	}

	store.SetFail(errors.New("store down"))
	if err := replicas[0].AllowTurn(ctx, domain.TenantContext{TenantID: 2}); err != nil {
		t.Fatalf("outage must fall back locally: %v", err)
	}
	if !replicas[0].Degraded() {
		t.Fatal("store outage must degrade")
	}
	if err := replicas[0].Close(); err != nil {
		t.Fatal(err)
	}
	if err := replicas[0].AllowTurn(ctx, domain.TenantContext{TenantID: 2}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("closed = %v", err)
	}
}

func TestDistributedTenantRateLimiterRequiresAnAtomicStore(t *testing.T) {
	if _, err := NewDistributedTenantRateLimiter(plainStore{}, nil, distributedConfig()); err == nil {
		t.Fatal("a store without MultiScopeAdmitter must be rejected")
	}
}

type plainStore struct{ cache.CacheService }

// Scout translates its config to keel's; a lane or bound Scout accepts that keel would reject must fail here.
func TestDistributedTenantRateLimiterRejectsInvalidConfig(t *testing.T) {
	cases := map[string]func(*DistributedRateLimiterConfig){
		"partial limit":         func(c *DistributedRateLimiterConfig) { c.Turn = WindowLimit{Limit: 1} },
		"negative limit":        func(c *DistributedRateLimiterConfig) { c.Tool = WindowLimit{Limit: -1, Window: time.Second} },
		"empty prefix":          func(c *DistributedRateLimiterConfig) { c.KeyPrefix = "" },
		"zero store timeout":    func(c *DistributedRateLimiterConfig) { c.StoreTimeout = 0 },
		"zero recovery":         func(c *DistributedRateLimiterConfig) { c.RecoveryProbe = 0 },
		"fraction above one":    func(c *DistributedRateLimiterConfig) { c.FallbackFraction = 1.5 },
		"zero fallback tenants": func(c *DistributedRateLimiterConfig) { c.FallbackMaxTenants = 0 },
	}
	for name, mutate := range cases {
		config := distributedConfig()
		mutate(&config)
		if _, err := NewDistributedTenantRateLimiter(&fake.CacheService{}, nil, config); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}
