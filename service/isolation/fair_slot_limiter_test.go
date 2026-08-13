package isolation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestFairSlotLimiterGrantsAndReleases(t *testing.T) {
	limiter := &FairSlotLimiter{Capacity: 2}
	ctx := context.Background()
	tenant := domain.TenantContext{TenantID: 1}

	first, err := limiter.acquire(ctx, tenant, 2)
	if err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() {
		lease, err := limiter.Acquire(ctx, tenant)
		if err == nil {
			lease.Release()
		}
		blocked <- err
	}()
	select {
	case err := <-blocked:
		t.Fatalf("second acquire should block, got %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := <-blocked; err != nil {
		t.Fatal(err)
	}
	// Double release must not free extra capacity.
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if lease, err := limiter.acquire(ctx, tenant, 2); err != nil {
		t.Fatal(err)
	} else {
		lease.Release()
	}
}

func TestFairSlotLimiterRoundRobinAcrossTenants(t *testing.T) {
	limiter := &FairSlotLimiter{Capacity: 1}
	ctx := context.Background()
	hold, err := limiter.Acquire(ctx, domain.TenantContext{TenantID: 1})
	if err != nil {
		t.Fatal(err)
	}

	order := make(chan int64, 3)
	acquire := func(tenantID int64) {
		lease, err := limiter.Acquire(ctx, domain.TenantContext{TenantID: tenantID})
		if err != nil {
			t.Error(err)
			return
		}
		order <- tenantID
		lease.Release()
	}
	// Tenant 1 queues two more requests, tenant 2 queues one.
	go acquire(1)
	time.Sleep(10 * time.Millisecond)
	go acquire(1)
	time.Sleep(10 * time.Millisecond)
	go acquire(2)
	time.Sleep(10 * time.Millisecond)

	hold.Release()
	got := []int64{<-order, <-order, <-order}
	// Round-robin serves tenant 2 before tenant 1's second queued request.
	if got[0] != 1 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("grant order = %v", got)
	}
}

func TestFairSlotLimiterCancellationDoesNotLeak(t *testing.T) {
	limiter := &FairSlotLimiter{Capacity: 1}
	hold, err := limiter.Acquire(context.Background(), domain.TenantContext{TenantID: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(ctx, domain.TenantContext{TenantID: 2}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled acquire = %v", err)
	}
	hold.Release()
	lease, err := limiter.Acquire(context.Background(), domain.TenantContext{TenantID: 3})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestFairSlotLimiterValidation(t *testing.T) {
	limiter := &FairSlotLimiter{Capacity: 2}
	ctx := context.Background()
	if _, err := limiter.Acquire(ctx, domain.TenantContext{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("tenant = %v", err)
	}
	if _, err := limiter.acquire(ctx, domain.TenantContext{TenantID: 1}, 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("weight = %v", err)
	}
	if _, err := limiter.acquire(ctx, domain.TenantContext{TenantID: 1}, 3); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversized = %v", err)
	}
}

func TestSlotCapacitySchedulerAdaptsLease(t *testing.T) {
	scheduler := &SlotCapacityScheduler{Slots: &FairSlotLimiter{Capacity: 1}, Pool: "shared"}
	request := domain.ModelRequest{TenantContext: domain.TenantContext{TenantID: 1}, RequestID: "r"}
	lease, err := scheduler.Acquire(context.Background(), request, domain.ModelSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Pool() != "shared" {
		t.Fatalf("pool = %q", lease.Pool())
	}
	if err := lease.Release(context.Background(), domain.Usage{}); err != nil {
		t.Fatal(err)
	}
	again, err := scheduler.Acquire(context.Background(), request, domain.ModelSelection{})
	if err != nil {
		t.Fatal(err)
	}
	again.Release(context.Background(), domain.Usage{})
}
