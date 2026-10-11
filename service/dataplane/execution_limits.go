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

func narrowedDuration(limit, requested time.Duration) time.Duration {
	if requested > 0 && requested < limit {
		return requested
	}
	return limit
}
