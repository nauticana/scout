package toolgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestRetryPolicyBoundsExponentialBackoff(t *testing.T) {
	// Rand pinned to 1 exposes the undithered upper bound.
	policy := RetryPolicy{MaxAttempts: 5, BaseDelay: 10 * time.Millisecond, MaxDelay: 25 * time.Millisecond, Rand: func() float64 { return 1 }}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 25 * time.Millisecond, 25 * time.Millisecond}
	for attempt, wantDelay := range want {
		delay, retry := policy.NextDelay(context.Background(), domain.ToolCall{}, domain.ToolResult{Retryable: true}, errors.New("failed"), attempt+1)
		if !retry || delay != wantDelay {
			t.Fatalf("attempt %d = %s/%v, want %s/true", attempt+1, delay, retry, wantDelay)
		}
	}
	if _, retry := policy.NextDelay(context.Background(), domain.ToolCall{}, domain.ToolResult{Retryable: true}, errors.New("failed"), 5); retry {
		t.Fatal("retry allowed after max attempts")
	}
}

func TestRetryPolicyRequiresFailureOrRetryableResult(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 2}
	if _, retry := policy.NextDelay(context.Background(), domain.ToolCall{}, domain.ToolResult{}, nil, 1); retry {
		t.Fatal("successful result must not retry")
	}
	if _, retry := policy.NextDelay(context.Background(), domain.ToolCall{}, domain.ToolResult{Retryable: true}, nil, 1); !retry {
		t.Fatal("retryable result was not retried")
	}
}

func TestRetryPolicyAppliesFullJitter(t *testing.T) {
	policy := RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, Rand: func() float64 { return 0.5 }}
	delay, retry := policy.NextDelay(context.Background(), domain.ToolCall{}, domain.ToolResult{Retryable: true}, errors.New("failed"), 1)
	if !retry || delay != 5*time.Millisecond {
		t.Fatalf("jittered = %s/%v", delay, retry)
	}
}
