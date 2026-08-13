package toolgateway

import (
	"context"
	"math"
	"math/rand/v2"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const maxDuration time.Duration = 1<<63 - 1

// RetryPolicy provides bounded exponential backoff with full jitter,
// so a shared dependency blip cannot re-synchronize the fleet's retries.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// Rand returns [0,1) for jitter; nil uses math/rand, tests inject a constant.
	Rand func() float64
}

// NextDelay returns the jittered delay before the next permitted attempt.
func (policy RetryPolicy) NextDelay(ctx context.Context, _ domain.ToolCall, result domain.ToolResult, _ error, attempt int) (time.Duration, bool) {
	if ctx.Err() != nil || policy.BaseDelay < 0 || policy.MaxDelay < 0 || attempt < 1 || attempt >= policy.MaxAttempts || !result.Retryable {
		return 0, false
	}
	if policy.BaseDelay <= 0 {
		return 0, true
	}
	delay := policy.BaseDelay
	for i := 1; i < attempt; i++ {
		if policy.MaxDelay > 0 && delay >= policy.MaxDelay/2 {
			delay = policy.MaxDelay
			break
		}
		if delay > maxDuration/2 {
			delay = maxDuration
			break
		}
		delay *= 2
	}
	if policy.MaxDelay > 0 && delay > policy.MaxDelay {
		delay = policy.MaxDelay
	}
	random := policy.Rand
	if random == nil {
		random = rand.Float64
	}
	fraction := random()
	if math.IsNaN(fraction) || fraction <= 0 {
		return 0, true
	}
	if fraction >= 1 {
		return delay, true
	}
	return time.Duration(fraction * float64(delay)), true
}

var _ contract.ToolRetryPolicy = RetryPolicy{}
