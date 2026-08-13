package isolation

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

type sharedCounterFake struct {
	mu     sync.Mutex
	counts map[string]int64
	calls  atomic.Int32
	fail   atomic.Bool
}

func (counter *sharedCounterFake) IncrementByWithTTL(_ context.Context, key string, n int64, _ time.Duration) (int64, error) {
	counter.calls.Add(1)
	if counter.fail.Load() {
		return 0, errors.New("store down")
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.counts == nil {
		counter.counts = make(map[string]int64)
	}
	counter.counts[key] += n
	return counter.counts[key], nil
}

func newDistLimiter(store *sharedCounterFake, global, local int64) *DistributedRateLimiter {
	return &DistributedRateLimiter{
		Store: store, GlobalLimit: global, LocalLimit: local,
		Window: time.Minute, Cooldown: 50 * time.Millisecond,
	}
}

func TestDistributedRateLimiterLocalGate(t *testing.T) {
	limiter := newDistLimiter(&sharedCounterFake{}, 100, 2)
	ctx := context.Background()
	if err := limiter.Allow(ctx, "k", 2); err != nil {
		t.Fatal(err)
	}
	err := limiter.Allow(ctx, "k", 1)
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Scope != "distributed.local" {
		t.Fatalf("local gate = %v", err)
	}
}

func TestDistributedRateLimiterGlobalPrefixAdmission(t *testing.T) {
	store := &sharedCounterFake{counts: map[string]int64{"k": 8}}
	limiter := newDistLimiter(store, 10, 100)
	ctx := context.Background()
	// Count is 8 of 10: a cost of 2 fits, the next cost of 1 exceeds.
	if err := limiter.Allow(ctx, "k", 2); err != nil {
		t.Fatal(err)
	}
	err := limiter.Allow(ctx, "k", 1)
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Scope != "distributed.global" {
		t.Fatalf("global gate = %v", err)
	}
}

func TestDistributedRateLimiterCoalescesHotKey(t *testing.T) {
	store := &sharedCounterFake{}
	limiter := newDistLimiter(store, 1000, 1000)
	limiter.BatchDelay = 20 * time.Millisecond
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := limiter.Allow(ctx, "hot", 1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls := store.calls.Load(); calls > 3 {
		t.Fatalf("store calls = %d, want coalescing", calls)
	}
}

func TestDistributedRateLimiterFallsBackAndRecovers(t *testing.T) {
	store := &sharedCounterFake{}
	limiter := newDistLimiter(store, 1, 100)
	ctx := context.Background()

	store.fail.Store(true)
	// Store failure: local admission is the authority even past the global limit.
	for range 3 {
		if err := limiter.Allow(ctx, "k", 1); err != nil {
			t.Fatalf("outage admission = %v", err)
		}
	}
	openCalls := store.calls.Load()
	if err := limiter.Allow(ctx, "k", 1); err != nil {
		t.Fatal(err)
	}
	if store.calls.Load() != openCalls {
		t.Fatal("open circuit still called the store")
	}

	// After cooldown one probe reaches the healthy store and closes the circuit.
	store.fail.Store(false)
	time.Sleep(60 * time.Millisecond)
	if err := limiter.Allow(ctx, "k", 1); err != nil {
		t.Fatalf("probe = %v", err)
	}
	err := limiter.Allow(ctx, "k", 1)
	if !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("recovered enforcement = %v", err)
	}
}

func TestDistributedRateLimiterValidation(t *testing.T) {
	limiter := &DistributedRateLimiter{}
	if err := limiter.Allow(context.Background(), "k", 1); err == nil {
		t.Fatal("missing configuration must error")
	}
	configured := newDistLimiter(&sharedCounterFake{}, 10, 10)
	if err := configured.Allow(context.Background(), "", 0); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
}

func TestDistributedRateLimiterBoundsLocalKeys(t *testing.T) {
	limiter := newDistLimiter(&sharedCounterFake{}, 100, 100)
	limiter.MaxKeys = 1
	if err := limiter.Allow(context.Background(), "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Allow(context.Background(), "b", 1); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("key capacity = %v", err)
	}
}
