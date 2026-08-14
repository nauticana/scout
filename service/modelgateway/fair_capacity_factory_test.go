package modelgateway

import (
	"context"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestNewFairCapacitySchedulerBuildsLease(t *testing.T) {
	scheduler, err := NewFairCapacityScheduler(FairCapacityConfig{Pool: " shared ", Slots: 1, MaxWaiters: 4096})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := scheduler.Acquire(context.Background(), domain.ModelRequest{TenantContext: domain.TenantContext{TenantID: 1}}, domain.ModelSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Pool() != "shared" {
		t.Fatalf("pool = %q", lease.Pool())
	}
	if err = lease.Release(context.Background(), domain.Usage{}); err != nil {
		t.Fatal(err)
	}
}

func TestNewFairCapacitySchedulerRejectsInvalidConfig(t *testing.T) {
	if _, err := NewFairCapacityScheduler(FairCapacityConfig{}); err == nil {
		t.Fatal("invalid capacity config must fail")
	}
}
