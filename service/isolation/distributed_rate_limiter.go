package isolation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DistributedRateLimiter enforces a fleet-wide fixed-window limit through a
// shared counter while staying available when the store is slow or down: the
// local per-replica limit is both the fast rejection gate and the outage
// authority, and hot keys coalesce into one store call per batch window.
// Worst-case overshoot during an outage is replicas × LocalLimit.
type DistributedRateLimiter struct {
	Store contract.SharedCounter
	// GlobalLimit is the fleet budget per window; LocalLimit the per-replica fallback.
	GlobalLimit, LocalLimit int64
	Window                  time.Duration
	// StoreTimeout bounds one counter call; default 50ms.
	StoreTimeout time.Duration
	// Cooldown holds the circuit open after a store failure; default 5s.
	Cooldown time.Duration
	// BatchDelay is the coalescing window per key; default 1ms.
	BatchDelay time.Duration
	// MaxKeys bounds replica-local windows; default 4096.
	MaxKeys int
	Now     func() time.Time

	mu         sync.Mutex
	local      map[string]localWindow
	batches    map[string]*counterBatch
	downUntil  time.Time
	probe      bool
	generation uint64
}

type localWindow struct {
	start time.Time
	count int64
}

type counterRequest struct {
	cost   int64
	result chan bool
}

type counterBatch struct{ requests []*counterRequest }

func (limiter *DistributedRateLimiter) defaults() (time.Duration, time.Duration, time.Duration, func() time.Time) {
	storeTimeout, cooldown, batchDelay, now := limiter.StoreTimeout, limiter.Cooldown, limiter.BatchDelay, limiter.Now
	if storeTimeout <= 0 {
		storeTimeout = 50 * time.Millisecond
	}
	if cooldown <= 0 {
		cooldown = 5 * time.Second
	}
	if batchDelay <= 0 {
		batchDelay = time.Millisecond
	}
	if now == nil {
		now = time.Now
	}
	return storeTimeout, cooldown, batchDelay, now
}

// Allow admits n units under key or returns retry advice.
func (limiter *DistributedRateLimiter) Allow(ctx context.Context, key string, n int) error {
	if limiter.Store == nil || limiter.GlobalLimit <= 0 || limiter.LocalLimit <= 0 || limiter.Window <= 0 {
		return fmt.Errorf("distributed rate limiter: store, window, global, and local limits are required")
	}
	key = strings.TrimSpace(key)
	if key == "" || n <= 0 {
		return fmt.Errorf("%w: key and positive cost are required", domain.ErrValidation)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _, _, nowFn := limiter.defaults()
	now := nowFn()

	// The local counter rejects cheaply before any shared-store traffic.
	limiter.mu.Lock()
	if limiter.local == nil {
		limiter.local = make(map[string]localWindow)
		limiter.batches = make(map[string]*counterBatch)
	}
	window := limiter.local[key]
	start := now.Truncate(limiter.Window)
	if !window.start.Equal(start) {
		if window.start.IsZero() && len(limiter.local) >= limiter.maxKeys() {
			limiter.sweepLocalLocked(start)
			if len(limiter.local) >= limiter.maxKeys() {
				limiter.mu.Unlock()
				return &LimitError{Err: domain.ErrRateLimited, Scope: "distributed.capacity", After: limiter.Window}
			}
		}
		window = localWindow{start: start}
	}
	cost := int64(n)
	if cost > limiter.LocalLimit || window.count > limiter.LocalLimit-cost {
		limiter.mu.Unlock()
		return &LimitError{Err: domain.ErrRateLimited, Scope: "distributed.local", After: start.Add(limiter.Window).Sub(now)}
	}
	window.count += cost
	limiter.local[key] = window

	// One goroutine per hot key serializes its store calls for every joiner.
	request := &counterRequest{cost: cost, result: make(chan bool, 1)}
	batch := limiter.batches[key]
	if batch == nil {
		batch = &counterBatch{}
		limiter.batches[key] = batch
		go limiter.flushLoop(key, batch)
	}
	batch.requests = append(batch.requests, request)
	limiter.mu.Unlock()

	select {
	case allowed := <-request.result:
		if !allowed {
			return &LimitError{Err: domain.ErrRateLimited, Scope: "distributed.global", After: limiter.Window}
		}
		return nil
	case <-ctx.Done():
		// The shared batch keeps running for the other joiners.
		return ctx.Err()
	}
}

func (limiter *DistributedRateLimiter) maxKeys() int {
	if limiter.MaxKeys > 0 {
		return limiter.MaxKeys
	}
	return defaultMaxTenants
}

func (limiter *DistributedRateLimiter) sweepLocalLocked(current time.Time) {
	for key, window := range limiter.local {
		if !window.start.Equal(current) {
			delete(limiter.local, key)
		}
	}
}

func (limiter *DistributedRateLimiter) flushLoop(key string, batch *counterBatch) {
	_, _, batchDelay, nowFn := limiter.defaults()
	for {
		time.Sleep(batchDelay)
		limiter.mu.Lock()
		requests := batch.requests
		batch.requests = nil
		useStore, generation := limiter.storePermitLocked(nowFn())
		limiter.mu.Unlock()

		if !useStore || limiter.Store == nil {
			// Local admission already happened, so an open circuit admits.
			for _, request := range requests {
				request.result <- true
			}
		} else {
			limiter.flushStore(key, requests, generation)
		}

		limiter.mu.Lock()
		if len(batch.requests) == 0 {
			delete(limiter.batches, key)
			limiter.mu.Unlock()
			return
		}
		limiter.mu.Unlock()
	}
}

func (limiter *DistributedRateLimiter) flushStore(key string, requests []*counterRequest, generation uint64) {
	storeTimeout, _, _, _ := limiter.defaults()
	var total int64
	for _, request := range requests {
		total += request.cost
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	count, err := limiter.Store.IncrementByWithTTL(ctx, key, total, limiter.Window)
	cancel()
	if err == nil && count < total {
		err = fmt.Errorf("shared counter returned %d after incrementing by %d", count, total)
	}
	limiter.storeDone(generation, err)

	// Store failure falls back to local admission; success admits the prefix under the limit.
	if err != nil {
		for _, request := range requests {
			request.result <- true
		}
		return
	}
	before := count - total
	for _, request := range requests {
		before += request.cost
		request.result <- before <= limiter.GlobalLimit
	}
}

func (limiter *DistributedRateLimiter) storePermitLocked(now time.Time) (bool, uint64) {
	if limiter.downUntil.IsZero() {
		return true, limiter.generation
	}
	if now.Before(limiter.downUntil) || limiter.probe {
		return false, limiter.generation
	}
	// An open circuit past its cooldown permits exactly one recovery probe.
	limiter.probe = true
	return true, limiter.generation
}

func (limiter *DistributedRateLimiter) storeDone(generation uint64, err error) {
	_, cooldown, _, nowFn := limiter.defaults()
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if err != nil {
		// Generations keep a stale in-flight success from hiding a newer failure.
		limiter.generation++
		limiter.downUntil = nowFn().Add(cooldown)
		limiter.probe = false
	} else if generation == limiter.generation {
		limiter.downUntil = time.Time{}
		limiter.probe = false
	}
}
