package dataplane

import (
	"time"

	"github.com/nauticana/scout/domain"
)

// narrowRuntimePolicy lowers the tenant's steps per turn and turn timeout to the release's
// limits; a release only narrows them.
func narrowRuntimePolicy(policy domain.TenantRuntimePolicy, limits domain.ExecutionLimits) domain.TenantRuntimePolicy {
	policy.MaxSteps = narrowed(policy.MaxSteps, limits.MaxSteps)
	policy.TurnTimeout = narrowedDuration(policy.TurnTimeout, limits.MaxDuration)
	return policy
}

// narrowLoopLimits lowers a tool-loop step's tool calls and deadline to the release's limits;
// no step runs longer than its whole turn may.
func narrowLoopLimits(loop domain.ToolLoopLimits, limits domain.ExecutionLimits) domain.ToolLoopLimits {
	loop.MaxToolCalls = narrowed(loop.MaxToolCalls, limits.MaxToolCalls)
	loop.Deadline = narrowedDuration(loop.Deadline, limits.MaxDuration)
	return loop
}

// narrowRuntimeBudget lowers the turn's token and cost ceilings to a per-turn release budget.
// The governor reads a zero cost ceiling as no cost allowed, so only a smaller positive value narrows.
func narrowRuntimeBudget(policy domain.TenantRuntimePolicy, budget domain.ReleaseBudget) (domain.TenantRuntimePolicy, error) {
	if budget.Period.Kind != domain.BudgetPerTurn || !budget.Bounded() {
		return policy, nil
	}
	policy.MaxTokens = narrowed64(policy.MaxTokens, budget.Tokens)
	if budget.CostMinorUnits > 0 {
		if budget.Currency != policy.CostCurrency {
			return policy, &domain.BudgetUnenforceableError{Problem: domain.BudgetCurrencyMismatch,
				Detail: "the per-turn cost ceiling is in " + budget.Currency + ", the turn in " + policy.CostCurrency}
		}
		policy.MaxCostMinorUnits = narrowed64(policy.MaxCostMinorUnits, budget.CostMinorUnits)
	}
	return policy, nil
}

// stepSpend is what the next step may still spend of a per-turn budget. A spent axis refuses the
// step: zero would read as unlimited.
func stepSpend(execution *turnExecution) (domain.StepSpend, error) {
	return execution.budget.StepSpend(execution.usage)
}

func narrowedDuration(limit, requested time.Duration) time.Duration {
	if requested > 0 && requested < limit {
		return requested
	}
	return limit
}

// positiveOr is a configured value, or its default when the value is not positive.
func positiveOr[T time.Duration | int | int64](value, fallback T) T {
	if value > 0 {
		return value
	}
	return fallback
}
