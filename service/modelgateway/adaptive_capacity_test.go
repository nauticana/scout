package modelgateway

import (
	"context"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func adaptiveRequest() domain.ModelRequest {
	return domain.ModelRequest{TenantContext: domain.TenantContext{TenantID: 1}, RequestID: "r"}
}

func TestAdaptiveCapacityGrowsOnHealthyLatency(t *testing.T) {
	now := time.Unix(0, 0)
	scheduler := &AdaptiveCapacityScheduler{Pool: "shared", Min: 2, Max: 4, Now: func() time.Time { return now }}
	ctx := context.Background()

	// Windows of 2 then 3 healthy samples raise the limit twice.
	for range 5 {
		lease, err := scheduler.Acquire(ctx, adaptiveRequest(), domain.ModelSelection{})
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(100 * time.Millisecond)
		lease.Release(ctx, domain.Usage{})
	}
	if scheduler.Limit() != 4 {
		t.Fatalf("limit = %d, want growth to 4", scheduler.Limit())
	}
}

func TestAdaptiveCapacityHalvesOnSlowWindow(t *testing.T) {
	now := time.Unix(0, 0)
	scheduler := &AdaptiveCapacityScheduler{Pool: "shared", Min: 1, Max: 8, Now: func() time.Time { return now }}
	ctx := context.Background()

	fast := func() {
		lease, _ := scheduler.Acquire(ctx, adaptiveRequest(), domain.ModelSelection{})
		now = now.Add(10 * time.Millisecond)
		lease.Release(ctx, domain.Usage{})
	}
	slow := func() {
		lease, _ := scheduler.Acquire(ctx, adaptiveRequest(), domain.ModelSelection{})
		now = now.Add(time.Second)
		lease.Release(ctx, domain.Usage{})
	}
	fast()
	fast()
	limit := scheduler.Limit()
	for range 2 * limit {
		slow()
	}
	if scheduler.Limit() != 1 {
		t.Fatalf("limit = %d, want collapse to min", scheduler.Limit())
	}
}

func TestAdaptiveCapacityBlocksAtLimitAndReleasesOnce(t *testing.T) {
	scheduler := &AdaptiveCapacityScheduler{Pool: "shared", Min: 1, Max: 1}
	ctx := context.Background()
	lease, err := scheduler.Acquire(ctx, adaptiveRequest(), domain.ModelSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Pool() != "shared" {
		t.Fatalf("pool = %q", lease.Pool())
	}
	blockedCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := scheduler.Acquire(blockedCtx, adaptiveRequest(), domain.ModelSelection{}); err == nil {
		t.Fatal("second acquire should block until timeout")
	}
	lease.Release(ctx, domain.Usage{})
	lease.Release(ctx, domain.Usage{})
	next, err := scheduler.Acquire(ctx, adaptiveRequest(), domain.ModelSelection{})
	if err != nil {
		t.Fatal(err)
	}
	next.Release(ctx, domain.Usage{})
}
