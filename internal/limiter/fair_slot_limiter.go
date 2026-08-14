package limiter

import (
	"context"
	"fmt"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// FairSlotLimiter is a weighted semaphore with per-tenant FIFOs served round-robin,
// so no tenant monopolizes capacity and a large request cannot starve behind small ones.
type FairSlotLimiter struct {
	Capacity int
	// MaxWaiters bounds queued acquisitions.
	MaxWaiters int

	mu      sync.Mutex
	inUse   int
	waiting int
	tenants map[int64]*slotQueue
	ready   []*slotQueue
}

var _ contract.ConcurrencyLimiter = (*FairSlotLimiter)(nil)

type slotQueue struct {
	tenant  int64
	waiters []*slotWaiter
}

type slotWaiter struct {
	weight  int
	ready   chan struct{}
	granted bool
}

// Acquire blocks until this tenant receives one slot or ctx is canceled.
func (limiter *FairSlotLimiter) Acquire(ctx context.Context, tenant domain.TenantContext) (contract.ConcurrencyLease, error) {
	return limiter.acquire(ctx, tenant, 1)
}

func (limiter *FairSlotLimiter) acquire(ctx context.Context, tenant domain.TenantContext, weight int) (contract.ConcurrencyLease, error) {
	if limiter.Capacity <= 0 {
		return nil, fmt.Errorf("fair slot limiter: capacity must be positive")
	}
	if limiter.MaxWaiters <= 0 {
		return nil, fmt.Errorf("fair slot limiter: max waiters must be positive")
	}
	if tenant.TenantID <= 0 || weight <= 0 {
		return nil, fmt.Errorf("%w: tenant and positive weight are required", domain.ErrValidation)
	}
	if weight > limiter.Capacity {
		return nil, fmt.Errorf("%w: weight %d exceeds capacity %d", domain.ErrValidation, weight, limiter.Capacity)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	waiter := &slotWaiter{weight: weight, ready: make(chan struct{})}
	limiter.mu.Lock()
	if limiter.tenants == nil {
		limiter.tenants = make(map[int64]*slotQueue)
	}
	if limiter.waiting >= limiter.MaxWaiters {
		limiter.mu.Unlock()
		return nil, fmt.Errorf("%w: concurrency queue capacity reached", domain.ErrRateLimited)
	}
	queue := limiter.tenants[tenant.TenantID]
	if queue == nil {
		queue = &slotQueue{tenant: tenant.TenantID}
		limiter.tenants[tenant.TenantID] = queue
		limiter.ready = append(limiter.ready, queue)
	}
	queue.waiters = append(queue.waiters, waiter)
	limiter.waiting++
	limiter.schedule()
	granted := waiter.granted
	limiter.mu.Unlock()

	lease := &slotLease{limiter: limiter, weight: weight}
	if granted {
		return lease, nil
	}
	select {
	case <-waiter.ready:
		return lease, nil
	case <-ctx.Done():
		// Resolve cancellation against a concurrent grant under the lock so capacity cannot leak.
		limiter.mu.Lock()
		if waiter.granted {
			limiter.mu.Unlock()
			return lease, nil
		}
		limiter.remove(queue, waiter)
		limiter.schedule()
		limiter.mu.Unlock()
		return nil, ctx.Err()
	}
}

// schedule grants one FIFO head per tenant turn; a blocked head reserves freed capacity.
func (limiter *FairSlotLimiter) schedule() {
	for len(limiter.ready) > 0 {
		queue := limiter.ready[0]
		waiter := queue.waiters[0]
		if waiter.weight > limiter.Capacity-limiter.inUse {
			return
		}
		limiter.ready = limiter.ready[1:]
		queue.waiters[0] = nil
		queue.waiters = queue.waiters[1:]
		if len(queue.waiters) == 0 {
			delete(limiter.tenants, queue.tenant)
		} else {
			limiter.ready = append(limiter.ready, queue)
		}
		limiter.inUse += waiter.weight
		limiter.waiting--
		waiter.granted = true
		close(waiter.ready)
	}
}

func (limiter *FairSlotLimiter) remove(queue *slotQueue, target *slotWaiter) {
	for i, waiter := range queue.waiters {
		if waiter == target {
			queue.waiters = append(queue.waiters[:i], queue.waiters[i+1:]...)
			limiter.waiting--
			break
		}
	}
	if len(queue.waiters) > 0 {
		return
	}
	delete(limiter.tenants, queue.tenant)
	for i, candidate := range limiter.ready {
		if candidate == queue {
			limiter.ready = append(limiter.ready[:i], limiter.ready[i+1:]...)
			return
		}
	}
}

func (limiter *FairSlotLimiter) release(weight int) {
	limiter.mu.Lock()
	limiter.inUse -= weight
	limiter.schedule()
	limiter.mu.Unlock()
}

type slotLease struct {
	once    sync.Once
	limiter *FairSlotLimiter
	weight  int
}

// Release returns the weight exactly once.
func (lease *slotLease) Release() error {
	lease.once.Do(func() { lease.limiter.release(lease.weight) })
	return nil
}

var _ contract.ConcurrencyLease = (*slotLease)(nil)

// SlotCapacityScheduler adapts a FairSlotLimiter to contract.CapacityScheduler for one pool.
type SlotCapacityScheduler struct {
	Slots *FairSlotLimiter
	Pool  string
	// Weight derives a request's slot weight; nil charges one slot.
	Weight func(domain.ModelRequest) int
}

var _ contract.CapacityScheduler = (*SlotCapacityScheduler)(nil)

// Acquire reserves weighted inference capacity in this scheduler's pool.
func (scheduler *SlotCapacityScheduler) Acquire(ctx context.Context, request domain.ModelRequest, _ domain.ModelSelection) (contract.CapacityLease, error) {
	if scheduler.Slots == nil || scheduler.Pool == "" {
		return nil, fmt.Errorf("slot capacity scheduler: slots and pool are required")
	}
	weight := 1
	if scheduler.Weight != nil {
		weight = scheduler.Weight(request)
	}
	lease, err := scheduler.Slots.acquire(ctx, request.TenantContext, weight)
	if err != nil {
		return nil, err
	}
	return &capacityLease{pool: scheduler.Pool, slot: lease}, nil
}

type capacityLease struct {
	pool string
	slot contract.ConcurrencyLease
}

func (lease *capacityLease) Pool() string { return lease.pool }

func (lease *capacityLease) Release(ctx context.Context, _ domain.Usage) error {
	return lease.slot.Release()
}

var _ contract.CapacityLease = (*capacityLease)(nil)
