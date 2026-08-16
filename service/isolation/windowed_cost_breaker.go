package isolation

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/limiter"
)

// WindowedCostBreaker trips tenant, agent, and fleet scopes when cost inside a sliding window exceeds its limit.
type WindowedCostBreaker struct {
	// Limits are minor units per Window; zero disables that scope.
	TenantLimit, AgentLimit, FleetLimit int64
	Currency                            string
	Window                              time.Duration
	// Buckets sets window resolution; zero uses six.
	Buckets int
	// MaxEntries bounds tenant and agent windows.
	MaxEntries int
	Now        func() time.Time

	mu               sync.Mutex
	once             sync.Once
	fleet            *costWindow
	tenants          map[int64]*costWindow
	agents           map[string]*costWindow
	untrackedRecords int64
	untrackedCost    int64
}

var _ contract.CostCircuitBreaker = (*WindowedCostBreaker)(nil)

func (breaker *WindowedCostBreaker) init() error {
	if breaker.Window <= 0 {
		return fmt.Errorf("windowed cost breaker: window must be positive")
	}
	if breaker.TenantLimit < 0 || breaker.AgentLimit < 0 || breaker.FleetLimit < 0 || breaker.MaxEntries <= 0 || breaker.Buckets < 0 {
		return fmt.Errorf("windowed cost breaker: limits and buckets cannot be negative and max entries must be positive")
	}
	if breaker.TenantLimit > 0 || breaker.AgentLimit > 0 || breaker.FleetLimit > 0 {
		if len(breaker.Currency) != 3 {
			return fmt.Errorf("windowed cost breaker: currency is required")
		}
	}
	breaker.once.Do(func() {
		if breaker.Buckets <= 0 {
			breaker.Buckets = 6
		}
		if breaker.Now == nil {
			breaker.Now = time.Now
		}
		breaker.fleet = newCostWindow(breaker.Buckets)
		breaker.tenants = make(map[int64]*costWindow)
		breaker.agents = make(map[string]*costWindow)
	})
	if breaker.Window/time.Duration(breaker.Buckets) <= 0 {
		return fmt.Errorf("windowed cost breaker: window is too small for bucket count")
	}
	return nil
}

// Allow rejects work whose projected cost would breach a tripped scope.
func (breaker *WindowedCostBreaker) Allow(ctx context.Context, tenantID int64, agentID string, projectedCostMinorUnits int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := breaker.init(); err != nil {
		return err
	}
	if tenantID <= 0 || projectedCostMinorUnits < 0 || (breaker.AgentLimit > 0 && agentID == "") {
		return fmt.Errorf("%w: tenant and non-negative projected cost are required", domain.ErrValidation)
	}
	slot := breaker.Window / time.Duration(breaker.Buckets)
	now := breaker.Now()
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	retry := breaker.Window
	if breaker.FleetLimit > 0 && exceedsCost(breaker.fleet.total(now, slot), projectedCostMinorUnits, breaker.FleetLimit) {
		return &limiter.LimitError{Err: domain.ErrCircuitOpen, Scope: "cost.fleet", After: retry}
	}
	if breaker.TenantLimit > 0 {
		if w := breaker.tenants[tenantID]; w != nil && exceedsCost(w.total(now, slot), projectedCostMinorUnits, breaker.TenantLimit) {
			return &limiter.LimitError{Err: domain.ErrCircuitOpen, Scope: "cost.tenant", After: retry}
		}
	}
	if breaker.AgentLimit > 0 {
		if w := breaker.agents[agentKey(tenantID, agentID)]; w != nil && exceedsCost(w.total(now, slot), projectedCostMinorUnits, breaker.AgentLimit) {
			return &limiter.LimitError{Err: domain.ErrCircuitOpen, Scope: "cost.agent", After: retry}
		}
	}
	if err := breaker.reserveTrackingLocked(now, slot, tenantID, agentID); err != nil {
		return err
	}
	return nil
}

// Record preserves completed usage even when an exact scope cannot be retained.
func (breaker *WindowedCostBreaker) Record(ctx context.Context, tenantID int64, agentID string, usage domain.Usage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := breaker.init(); err != nil {
		return err
	}
	if tenantID <= 0 || usage.CostMinorUnits < 0 || (breaker.AgentLimit > 0 && agentID == "") {
		return fmt.Errorf("%w: tenant, agent, and non-negative cost are required", domain.ErrValidation)
	}
	if usage.CostMinorUnits == 0 {
		return nil
	}
	if usage.Currency != breaker.Currency {
		return fmt.Errorf("%w: cost currency does not match breaker currency", domain.ErrValidation)
	}
	slot := breaker.Window / time.Duration(breaker.Buckets)
	now := breaker.Now()
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	agentWindowKey := agentKey(tenantID, agentID)
	if (breaker.TenantLimit > 0 && breaker.tenants[tenantID] == nil) ||
		(breaker.AgentLimit > 0 && breaker.agents[agentWindowKey] == nil) {
		breaker.sweepLocked(now, slot)
	}
	var tenant, agent *costWindow
	if breaker.TenantLimit > 0 {
		tenant = breaker.tenants[tenantID]
		if tenant == nil {
			if len(breaker.tenants) < breaker.maxEntries() {
				tenant = newCostWindow(breaker.Buckets)
				tenant.protect(now, breaker.Window)
				breaker.tenants[tenantID] = tenant
			}
		}
	}
	if breaker.AgentLimit > 0 {
		agent = breaker.agents[agentWindowKey]
		if agent == nil {
			if len(breaker.agents) < breaker.maxEntries() {
				agent = newCostWindow(breaker.Buckets)
				agent.protect(now, breaker.Window)
				breaker.agents[agentWindowKey] = agent
			}
		}
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if breaker.FleetLimit > 0 && breaker.fleet.total(now, slot) > maxInt64-usage.CostMinorUnits ||
		tenant != nil && tenant.total(now, slot) > maxInt64-usage.CostMinorUnits ||
		agent != nil && agent.total(now, slot) > maxInt64-usage.CostMinorUnits {
		return fmt.Errorf("%w: cost window overflow", domain.ErrValidation)
	}
	if breaker.FleetLimit > 0 {
		breaker.fleet.add(now, slot, usage.CostMinorUnits)
	}
	if tenant != nil {
		tenant.add(now, slot, usage.CostMinorUnits)
	}
	if agent != nil {
		agent.add(now, slot, usage.CostMinorUnits)
	}
	if (breaker.TenantLimit > 0 && tenant == nil) || (breaker.AgentLimit > 0 && agent == nil) {
		if breaker.untrackedRecords < maxInt64 {
			breaker.untrackedRecords++
		}
		if breaker.untrackedCost <= maxInt64-usage.CostMinorUnits {
			breaker.untrackedCost += usage.CostMinorUnits
		} else {
			breaker.untrackedCost = maxInt64
		}
	}
	return nil
}

// Untracked reports completed records that could not retain an exact scope.
func (breaker *WindowedCostBreaker) Untracked() (records, costMinorUnits int64) {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()
	return breaker.untrackedRecords, breaker.untrackedCost
}

func (breaker *WindowedCostBreaker) reserveTrackingLocked(now time.Time, slot time.Duration, tenantID int64, agentID string) error {
	agentWindowKey := agentKey(tenantID, agentID)
	missingTenant := breaker.TenantLimit > 0 && breaker.tenants[tenantID] == nil
	missingAgent := breaker.AgentLimit > 0 && breaker.agents[agentWindowKey] == nil
	if missingTenant || missingAgent {
		breaker.sweepLocked(now, slot)
		missingTenant = breaker.TenantLimit > 0 && breaker.tenants[tenantID] == nil
		missingAgent = breaker.AgentLimit > 0 && breaker.agents[agentWindowKey] == nil
	}
	if missingTenant && len(breaker.tenants) >= breaker.maxEntries() {
		return &limiter.LimitError{Err: domain.ErrRateLimited, Scope: "cost.tenant.capacity", After: breaker.Window}
	}
	if missingAgent && len(breaker.agents) >= breaker.maxEntries() {
		return &limiter.LimitError{Err: domain.ErrRateLimited, Scope: "cost.agent.capacity", After: breaker.Window}
	}
	if missingTenant {
		breaker.tenants[tenantID] = newCostWindow(breaker.Buckets)
	}
	if missingAgent {
		breaker.agents[agentWindowKey] = newCostWindow(breaker.Buckets)
	}
	if breaker.TenantLimit > 0 {
		breaker.tenants[tenantID].protect(now, breaker.Window)
	}
	if breaker.AgentLimit > 0 {
		breaker.agents[agentWindowKey].protect(now, breaker.Window)
	}
	return nil
}

func (breaker *WindowedCostBreaker) maxEntries() int {
	return breaker.MaxEntries
}

// sweepLocked drops windows whose retained cost is zero, bounding both maps.
func (breaker *WindowedCostBreaker) sweepLocked(now time.Time, slot time.Duration) {
	for id, w := range breaker.tenants {
		if w.total(now, slot) == 0 && !now.Before(w.protectedUntil) {
			delete(breaker.tenants, id)
		}
	}
	for key, w := range breaker.agents {
		if w.total(now, slot) == 0 && !now.Before(w.protectedUntil) {
			delete(breaker.agents, key)
		}
	}
}

func agentKey(tenantID int64, agentID string) string {
	return strconv.FormatInt(tenantID, 10) + "/" + agentID
}

func exceedsCost(current, projected, limit int64) bool {
	return projected > limit || current > limit-projected
}
