package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// TenantRateLimiter protects the fleet from tenant traffic spikes.
type TenantRateLimiter interface {
	// AllowTurn enforces the tenant's admission rate and priority class.
	AllowTurn(ctx context.Context, tenant domain.TenantContext) error
	// AllowToolCall enforces the tenant's tool-call rate.
	AllowToolCall(ctx context.Context, call domain.ToolCall) error
	// AllowModelCall enforces the tenant's inference-call rate.
	AllowModelCall(ctx context.Context, request domain.ModelRequest) error
}

// TenantBudgetManager reserves and settles tenant token and cost budgets.
type TenantBudgetManager interface {
	// Reserve atomically reserves tokens and cost before work begins.
	Reserve(ctx context.Context, tenantID int64, requestID string, tokens, costMinorUnits int64, currency string) (domain.BudgetReservation, error)
	// Commit settles a reservation against actual usage.
	Commit(ctx context.Context, reservation domain.BudgetReservation, usage domain.Usage) error
	// Release returns an unused reservation to the tenant budget.
	Release(ctx context.Context, reservation domain.BudgetReservation) error
}

// ExecutionGovernor creates hard limits for one agent turn.
type ExecutionGovernor interface {
	// Start initializes hard limits for one tenant turn.
	Start(ctx context.Context, request domain.TurnRequest, policy domain.TenantRuntimePolicy) (ExecutionPermit, error)
}

// ExecutionPermit enforces limits throughout one agent turn.
type ExecutionPermit interface {
	// BeforeStep rejects work that exceeds time, step, token, or cost limits.
	BeforeStep(ctx context.Context, step domain.ExecutionStep) error
	// AfterStep accounts for usage and checks for repeated execution patterns.
	AfterStep(ctx context.Context, result domain.StepResult) error
	// Close releases reservations and records the terminal outcome.
	Close(ctx context.Context, usage domain.Usage) error
}

// LoopDetector identifies repeated agent behavior across execution steps.
type LoopDetector interface {
	// Observe reports an error when step history indicates a runaway loop.
	Observe(ctx context.Context, tenantID int64, conversationID, fingerprint string) error
	// Reset clears loop history after a turn reaches a terminal state.
	Reset(ctx context.Context, tenantID int64, conversationID string) error
}

// CostCircuitBreaker stops work when configured cost thresholds are exceeded.
type CostCircuitBreaker interface {
	// Allow rejects work after tenant, agent, or fleet cost thresholds trip.
	Allow(ctx context.Context, tenantID int64, agentID string, projectedCostMinorUnits int64) error
	// Record adds actual usage to tenant, agent, and fleet cost windows.
	Record(ctx context.Context, tenantID int64, agentID string, usage domain.Usage) error
}

// ConcurrencyLimiter isolates tenant execution concurrency.
type ConcurrencyLimiter interface {
	// Acquire reserves one tenant-scoped execution slot.
	Acquire(ctx context.Context, tenant domain.TenantContext) (ConcurrencyLease, error)
}

// ConcurrencyLease represents one reserved tenant execution slot.
type ConcurrencyLease interface {
	// Release returns the execution slot to its tenant capacity pool.
	Release() error
}
