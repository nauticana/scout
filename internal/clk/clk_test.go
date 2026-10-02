package clk

import (
	"context"
	"testing"
	"time"

	"github.com/nauticana/keel/clock"
)

func TestOfReadsTheInjectedClock(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	injected := Of(func() time.Time { return at })
	if !injected.Now().Equal(at) || injected.Since(at.Add(-time.Minute)) != time.Minute {
		t.Fatalf("Now = %v, Since = %v", injected.Now(), injected.Since(at.Add(-time.Minute)))
	}
	if _, system := Of(nil).(clock.System); !system {
		t.Fatalf("nil must be the system clock, got %T", Of(nil))
	}
}

// Waiting stays on real time, so a cancelled context ends a sleep at once.
func TestOfWaitsOnRealTime(t *testing.T) {
	injected := Of(func() time.Time { return time.Time{} })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := injected.Sleep(ctx, time.Hour); err == nil {
		t.Fatal("a cancelled sleep must return its context error")
	}
	timer := injected.NewTimer(time.Millisecond)
	select {
	case <-timer.C():
	case <-time.After(time.Second):
		t.Fatal("the timer must fire on real time")
	}
	ticker := injected.NewTicker(time.Millisecond)
	defer ticker.Stop()
	<-ticker.C()
}
