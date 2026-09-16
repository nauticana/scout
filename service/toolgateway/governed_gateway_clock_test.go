package toolgateway

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauticana/keel/clock"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// The retry wait must run on the injected clock: a real timer would make this
// test take an hour, and made the old wait untestable at all.
func TestGovernedGatewayRetryWaitIsDrivenByTheClock(t *testing.T) {
	fakeClock := clock.NewFake(time.Time{})
	var attempts atomic.Int32
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		if attempts.Add(1) == 1 {
			return domain.ToolResult{Retryable: true}, errors.New("temporary")
		}
		return domain.ToolResult{Output: []byte("ok")}, nil
	}))
	gateway.Retry = RetryPolicy{MaxAttempts: 2, BaseDelay: time.Hour, Rand: func() float64 { return 1 }}
	gateway.Clock = fakeClock

	done := make(chan error, 1)
	go func() {
		_, err := gateway.Invoke(context.Background(), validToolCall())
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for attempts.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("first attempt never ran")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("retry completed without the clock advancing: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// The timer may be created slightly after the first attempt returns, so keep
	// advancing until it fires.
	for {
		fakeClock.Advance(time.Hour)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if got := attempts.Load(); got != 2 {
				t.Fatalf("attempts = %d, want 2", got)
			}
			return
		case <-time.After(5 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("retry never resumed after the clock advanced")
			}
		}
	}
}
