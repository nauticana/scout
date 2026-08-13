package isolation

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/bucket"
)

// RateLimit configures one admission lane in calls per second.
type RateLimit struct {
	PerSecond float64
	Burst     int
}

func (l RateLimit) enabled() bool { return l.PerSecond > 0 && l.Burst > 0 }

func (l RateLimit) valid() bool {
	return l.PerSecond == 0 && l.Burst == 0 || l.enabled()
}

const (
	defaultMaxTenants   = 4096
	tenantSweepCooldown = 100 * time.Millisecond
)

// TenantRateLimiter enforces per-tenant and fleet admission rates for turns, tool calls, and model calls.
// A zero-valued RateLimit disables that check.
type TenantRateLimiter struct {
	Turn, Tool, Model                RateLimit
	FleetTurn, FleetTool, FleetModel RateLimit
	MaxTenants                       int
	Now                              func() time.Time

	once  sync.Once
	turns *lane
	tools *lane
	model *lane
}

var _ contract.TenantRateLimiter = (*TenantRateLimiter)(nil)

func (limiter *TenantRateLimiter) init() {
	limiter.once.Do(func() {
		now := limiter.Now
		if now == nil {
			now = time.Now
		}
		max := limiter.MaxTenants
		if max <= 0 {
			max = defaultMaxTenants
		}
		build := func(scope string, tenant, fleet RateLimit) *lane {
			l := &lane{scope: scope, tenant: tenant, max: max, now: now}
			if fleet.enabled() {
				l.fleet = bucket.New(fleet.PerSecond, float64(fleet.Burst), now())
			}
			return l
		}
		limiter.turns = build("turn", limiter.Turn, limiter.FleetTurn)
		limiter.tools = build("tool", limiter.Tool, limiter.FleetTool)
		limiter.model = build("model", limiter.Model, limiter.FleetModel)
	})
}

// AllowTurn enforces the tenant's turn admission rate.
func (limiter *TenantRateLimiter) AllowTurn(ctx context.Context, tenant domain.TenantContext) error {
	return limiter.allow(ctx, func() *lane { return limiter.turns }, tenant.TenantID)
}

// AllowToolCall enforces the tenant's tool-call rate.
func (limiter *TenantRateLimiter) AllowToolCall(ctx context.Context, call domain.ToolCall) error {
	return limiter.allow(ctx, func() *lane { return limiter.tools }, call.TenantContext.TenantID)
}

// AllowModelCall enforces the tenant's inference-call rate.
func (limiter *TenantRateLimiter) AllowModelCall(ctx context.Context, request domain.ModelRequest) error {
	return limiter.allow(ctx, func() *lane { return limiter.model }, request.TenantContext.TenantID)
}

func (limiter *TenantRateLimiter) allow(ctx context.Context, pick func() *lane, tenantID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID <= 0 {
		return fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	if limiter.MaxTenants < 0 || !limiter.Turn.valid() || !limiter.Tool.valid() || !limiter.Model.valid() ||
		!limiter.FleetTurn.valid() || !limiter.FleetTool.valid() || !limiter.FleetModel.valid() {
		return fmt.Errorf("tenant rate limiter: rates require positive rate and burst")
	}
	limiter.init()
	return pick().allow(tenantID)
}

// lane is one admission concern: tenant buckets plus an optional fleet bucket under one lock.
type lane struct {
	mu        sync.Mutex
	scope     string
	tenant    RateLimit
	fleet     *bucket.Bucket
	tenants   map[int64]*bucket.Bucket
	max       int
	nextSweep time.Time
	now       func() time.Time
}

func (l *lane) allow(tenantID int64) error {
	if !l.tenant.enabled() && l.fleet == nil {
		return nil
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	var tenantBucket *bucket.Bucket
	var stored bool
	if l.tenant.enabled() {
		if l.tenants == nil {
			l.tenants = make(map[int64]*bucket.Bucket)
		}
		tenantBucket, stored = l.tenants[tenantID]
		if !stored {
			// A drained bucket must never be evicted: a fresh replacement would be full,
			// so only fully-refilled buckets are swept and the map refuses beyond its cap.
			if len(l.tenants) >= l.max && !now.Before(l.nextSweep) {
				l.sweep(now)
				l.nextSweep = now.Add(tenantSweepCooldown)
			}
			if len(l.tenants) >= l.max {
				return &LimitError{Err: domain.ErrRateLimited, Scope: l.scope + ".capacity", After: tenantSweepCooldown}
			}
			tenantBucket = bucket.New(l.tenant.PerSecond, float64(l.tenant.Burst), now)
		}
		tenantBucket.Refill(now)
	}
	if l.fleet != nil {
		l.fleet.Refill(now)
	}

	if tenantBucket != nil && tenantBucket.Wait(1) > 0 {
		return &LimitError{Err: domain.ErrRateLimited, Scope: l.scope + ".tenant", After: tenantBucket.Wait(1)}
	}
	if l.fleet != nil && l.fleet.Wait(1) > 0 {
		return &LimitError{Err: domain.ErrRateLimited, Scope: l.scope + ".fleet", After: l.fleet.Wait(1)}
	}
	// Deduct both only after both can grant, so a denial leaks nothing.
	if tenantBucket != nil {
		tenantBucket.Take(1)
		if !stored {
			l.tenants[tenantID] = tenantBucket
		}
	}
	if l.fleet != nil {
		l.fleet.Take(1)
	}
	return nil
}

func (l *lane) sweep(now time.Time) {
	for tenantID, b := range l.tenants {
		if b.Refill(now); b.Full() {
			delete(l.tenants, tenantID)
		}
	}
}
