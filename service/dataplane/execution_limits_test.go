package dataplane

import (
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestReleaseLimitsOnlyNarrowTheTenantPolicyAndTheLoop(t *testing.T) {
	policy := domain.TenantRuntimePolicy{MaxSteps: 10, MaxTokens: 1000, CostCurrency: "USD", TurnTimeout: time.Minute}
	loop := domain.ToolLoopLimits{MaxIterations: 6, MaxToolCalls: 8, MaxTokens: 1000, MaxRepeatedCalls: 2, Deadline: time.Minute}
	cases := map[string]struct {
		limits              domain.ExecutionLimits
		steps, toolCalls    int
		timeout, loopWindow time.Duration
	}{
		"unbound": {steps: 10, toolCalls: 8, timeout: time.Minute, loopWindow: time.Minute},
		"lower": {limits: domain.ExecutionLimits{MaxSteps: 3, MaxToolCalls: 2, MaxDuration: 20 * time.Second},
			steps: 3, toolCalls: 2, timeout: 20 * time.Second, loopWindow: 20 * time.Second},
		"higher": {limits: domain.ExecutionLimits{MaxSteps: 50, MaxToolCalls: 50, MaxDuration: time.Hour},
			steps: 10, toolCalls: 8, timeout: time.Minute, loopWindow: time.Minute},
		"negative": {limits: domain.ExecutionLimits{MaxSteps: -1, MaxToolCalls: -1, MaxDuration: -time.Second},
			steps: 10, toolCalls: 8, timeout: time.Minute, loopWindow: time.Minute},
	}
	for name, test := range cases {
		narrowedPolicy := narrowRuntimePolicy(policy, test.limits)
		narrowedLoop := narrowLoopLimits(loop, test.limits)
		if narrowedPolicy.MaxSteps != test.steps || narrowedPolicy.TurnTimeout != test.timeout ||
			narrowedPolicy.MaxTokens != policy.MaxTokens || narrowedPolicy.CostCurrency != policy.CostCurrency {
			t.Errorf("%s: policy = %+v", name, narrowedPolicy)
		}
		if narrowedLoop.MaxToolCalls != test.toolCalls || narrowedLoop.Deadline != test.loopWindow ||
			narrowedLoop.MaxIterations != loop.MaxIterations || narrowedLoop.MaxTokens != loop.MaxTokens {
			t.Errorf("%s: loop = %+v", name, narrowedLoop)
		}
	}
}
