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

// TenantBudgetPolicy supplies each tenant's rolling-window budget.
type TenantBudgetPolicy interface {
	BudgetFor(ctx context.Context, tenantID int64) (domain.BudgetLimits, error)
}

// PrincipalBudgetPolicy supplies the rolling-window budget of one principal inside its
// tenant's. bounded false leaves the principal to the tenant envelope alone. The
// limits share the tenant's currency and may only narrow it: both must hold.
type PrincipalBudgetPolicy interface {
	PrincipalBudgetFor(ctx context.Context, tenantID int64, principal domain.PrincipalRef) (limits domain.BudgetLimits, bounded bool, err error)
}

// TenantBudgetManager reserves and settles tenant token and cost budgets.
type TenantBudgetManager interface {
	// Reserve returns the live attempt or replaces an expired attempt for a nonterminal turn.
	Reserve(ctx context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error)
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
	// Close releases resources and records the total terminal usage.
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
	// Allow rejects work before execution when a cost or tracking limit is reached.
	Allow(ctx context.Context, tenantID int64, agentID string, projectedCostMinorUnits int64) error
	// Record preserves actual usage after execution.
	Record(ctx context.Context, tenantID int64, agentID string, usage domain.Usage) error
}

// LatencyBudgetAllocator turns a request deadline into per-stage budgets,
// reserving generation first and rejecting turns whose minimum path cannot fit.
type LatencyBudgetAllocator interface {
	Allocate(ctx context.Context, request domain.TurnRequest, policy domain.TenantRuntimePolicy) (domain.TurnBudget, error)
}

// StageLatencyModel predicts per-stage cost for one request; the allocator reserves generation from it first.
type StageLatencyModel interface {
	Estimate(ctx context.Context, request domain.TurnRequest, policy domain.TenantRuntimePolicy) (domain.StageLatencyEstimate, error)
}
