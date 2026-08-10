package toolgateway

import (
	"context"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const maxDuration time.Duration = 1<<63 - 1

// RetryPolicy provides deterministic bounded exponential backoff.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// NextDelay returns the delay before the next permitted attempt.
func (policy RetryPolicy) NextDelay(ctx context.Context, _ domain.ToolCall, result domain.ToolResult, _ error, attempt int) (time.Duration, bool) {
	if ctx.Err() != nil || attempt < 1 || attempt >= policy.MaxAttempts || !result.Retryable {
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
	return delay, true
}

var _ contract.ToolRetryPolicy = RetryPolicy{}
