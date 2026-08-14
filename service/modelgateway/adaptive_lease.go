package modelgateway

import (
	"context"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type adaptiveLease struct {
	once      sync.Once
	scheduler *AdaptiveCapacityScheduler
	started   time.Time
}

func (lease *adaptiveLease) Pool() string { return lease.scheduler.Pool }

// Release frees the slot exactly once and records the call's latency.
func (lease *adaptiveLease) Release(context.Context, domain.Usage) error {
	lease.once.Do(func() { lease.scheduler.complete(lease.scheduler.Now().Sub(lease.started)) })
	return nil
}

var _ contract.CapacityLease = (*adaptiveLease)(nil)
