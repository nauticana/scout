package isolation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ExecutionGovernor creates per-turn permits with hard runtime limits.
type ExecutionGovernor struct {
	Loops contract.LoopDetector
	Costs contract.CostCircuitBreaker
	Now   func() time.Time
}

// Start validates policy and creates one isolated turn permit.
func (governor *ExecutionGovernor) Start(ctx context.Context, request domain.TurnRequest, policy domain.TenantRuntimePolicy) (contract.ExecutionPermit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if governor.Loops == nil || governor.Costs == nil {
		return nil, fmt.Errorf("execution governor: loop detector and cost circuit breaker are required")
	}
	if request.TenantContext.TenantID <= 0 || strings.TrimSpace(request.RequestID) == "" || strings.TrimSpace(request.ConversationID) == "" || strings.TrimSpace(request.AgentID) == "" {
		return nil, fmt.Errorf("%w: tenant, request, conversation, and agent are required", domain.ErrValidation)
	}
	if policy.MaxSteps <= 0 || policy.MaxTokens <= 0 || policy.MaxCostMinorUnits < 0 || policy.TurnTimeout <= 0 || len(strings.TrimSpace(policy.CostCurrency)) != 3 {
		return nil, fmt.Errorf("%w: runtime policy limits and currency are invalid", domain.ErrValidation)
	}
	now := governor.Now
	if now == nil {
		now = time.Now
	}
	return &executionPermit{
		loops:   governor.Loops,
		costs:   governor.Costs,
		now:     now,
		request: request,
		policy:  policy,
		started: now(),
	}, nil
}

type executionPermit struct {
	mu      sync.Mutex
	loops   contract.LoopDetector
	costs   contract.CostCircuitBreaker
	now     func() time.Time
	request domain.TurnRequest
	policy  domain.TenantRuntimePolicy
	started time.Time
	steps   int
	usage   domain.Usage
	pending bool
	closed  bool
}

func (permit *executionPermit) BeforeStep(ctx context.Context, step domain.ExecutionStep) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(step.StepID) == "" || strings.TrimSpace(step.Kind) == "" {
		return fmt.Errorf("%w: step id and kind are required", domain.ErrValidation)
	}
	permit.mu.Lock()
	if permit.closed {
		permit.mu.Unlock()
		return fmt.Errorf("%w: execution permit is closed", domain.ErrConflict)
	}
	if permit.pending {
		permit.mu.Unlock()
		return fmt.Errorf("%w: previous execution step is unfinished", domain.ErrConflict)
	}
	if permit.now().Sub(permit.started) >= permit.policy.TurnTimeout || permit.steps >= permit.policy.MaxSteps {
		permit.mu.Unlock()
		return domain.ErrExecutionLimit
	}
	permit.mu.Unlock()
	if err := permit.costs.Allow(ctx, permit.request.TenantContext.TenantID, permit.request.AgentID, 0); err != nil {
		return err
	}
	permit.mu.Lock()
	defer permit.mu.Unlock()
	if permit.closed || permit.pending {
		return fmt.Errorf("%w: execution permit state changed", domain.ErrConflict)
	}
	if permit.now().Sub(permit.started) >= permit.policy.TurnTimeout || permit.steps >= permit.policy.MaxSteps {
		return domain.ErrExecutionLimit
	}
	permit.steps++
	permit.pending = true
	return nil
}

func (permit *executionPermit) AfterStep(ctx context.Context, result domain.StepResult) error {
	if err := validateUsage(result.Usage, permit.policy.CostCurrency); err != nil {
		return err
	}
	permit.mu.Lock()
	if permit.closed {
		permit.mu.Unlock()
		return fmt.Errorf("%w: execution permit is closed", domain.ErrConflict)
	}
	if !permit.pending {
		permit.mu.Unlock()
		return fmt.Errorf("%w: no execution step is pending", domain.ErrConflict)
	}
	addUsage(&permit.usage, result.Usage)
	permit.pending = false
	total := permit.usage
	limitErr := permit.limitError(total)
	permit.mu.Unlock()

	accountingCtx := context.WithoutCancel(ctx)
	var loopErr error
	if result.Fingerprint != "" {
		loopErr = permit.loops.Observe(accountingCtx, permit.request.TenantContext.TenantID, permit.request.ConversationID, result.Fingerprint)
	}
	costErr := permit.costs.Record(accountingCtx, permit.request.TenantContext.TenantID, permit.request.AgentID, result.Usage)
	return errors.Join(ctx.Err(), limitErr, loopErr, costErr)
}

func (permit *executionPermit) Close(ctx context.Context, usage domain.Usage) error {
	if err := validateUsage(usage, permit.policy.CostCurrency); err != nil {
		return err
	}
	permit.mu.Lock()
	if permit.closed {
		permit.mu.Unlock()
		return nil
	}
	permit.closed = true
	tracked := permit.usage
	var terminalErr error
	if usage.InputTokens < tracked.InputTokens || usage.OutputTokens < tracked.OutputTokens || usage.ToolCalls < tracked.ToolCalls || usage.CostMinorUnits < tracked.CostMinorUnits {
		terminalErr = fmt.Errorf("%w: terminal usage is lower than accounted usage", domain.ErrConflict)
		usage = tracked
	}
	delta := subtractUsage(usage, tracked)
	limitErr := permit.limitError(usage)
	permit.mu.Unlock()

	accountingCtx := context.WithoutCancel(ctx)
	var costErr error
	if delta.InputTokens != 0 || delta.OutputTokens != 0 || delta.ToolCalls != 0 || delta.CostMinorUnits != 0 {
		costErr = permit.costs.Record(accountingCtx, permit.request.TenantContext.TenantID, permit.request.AgentID, delta)
	}
	loopErr := permit.loops.Reset(accountingCtx, permit.request.TenantContext.TenantID, permit.request.ConversationID)
	return errors.Join(ctx.Err(), terminalErr, limitErr, costErr, loopErr)
}

func (permit *executionPermit) limitError(usage domain.Usage) error {
	if permit.now().Sub(permit.started) >= permit.policy.TurnTimeout || permit.steps > permit.policy.MaxSteps || usage.InputTokens+usage.OutputTokens > permit.policy.MaxTokens {
		return domain.ErrExecutionLimit
	}
	if usage.CostMinorUnits > permit.policy.MaxCostMinorUnits {
		return domain.ErrBudgetExceeded
	}
	return nil
}

func validateUsage(usage domain.Usage, currency string) error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.CostMinorUnits < 0 {
		return fmt.Errorf("%w: usage cannot be negative", domain.ErrValidation)
	}
	if usage.Currency != "" && usage.Currency != currency {
		return fmt.Errorf("%w: usage currency does not match runtime policy", domain.ErrValidation)
	}
	if usage.CostMinorUnits > 0 && usage.Currency == "" {
		return fmt.Errorf("%w: cost currency is required", domain.ErrValidation)
	}
	return nil
}

func addUsage(total *domain.Usage, usage domain.Usage) {
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.ToolCalls += usage.ToolCalls
	total.CostMinorUnits += usage.CostMinorUnits
	if total.Currency == "" {
		total.Currency = usage.Currency
	}
}

func subtractUsage(total, accounted domain.Usage) domain.Usage {
	return domain.Usage{
		InputTokens:    total.InputTokens - accounted.InputTokens,
		OutputTokens:   total.OutputTokens - accounted.OutputTokens,
		ToolCalls:      total.ToolCalls - accounted.ToolCalls,
		CostMinorUnits: total.CostMinorUnits - accounted.CostMinorUnits,
		Currency:       total.Currency,
	}
}

var _ contract.ExecutionGovernor = (*ExecutionGovernor)(nil)
var _ contract.ExecutionPermit = (*executionPermit)(nil)
