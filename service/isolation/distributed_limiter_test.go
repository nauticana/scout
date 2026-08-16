package isolation

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func distributedConfig() DistributedRateLimiterConfig {
	return DistributedRateLimiterConfig{
		Turn:               WindowLimit{Limit: 4, Window: time.Second},
		FleetTurn:          WindowLimit{Limit: 6, Window: time.Second},
		KeyPrefix:          "scout:rl",
		StoreTimeout:       50 * time.Millisecond,
		FallbackFraction:   0.5,
		FallbackMaxTenants: 64,
		RecoveryProbe:      time.Second,
	}
}

type dependencyRecorder struct {
	fake.RuntimeMetrics
	mu           sync.Mutex
	errors       []error
	observations []domain.Observation
}

func (r *dependencyRecorder) RecordDependency(_ context.Context, _ int64, _, _ string, _ domain.Usage, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, err)
}

func (r *dependencyRecorder) RecordObservation(_ context.Context, observation domain.Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observations = append(r.observations, observation)
}

func newReplicas(t *testing.T, store *fake.CacheService, metrics *dependencyRecorder, count int, now func() time.Time) []*DistributedTenantRateLimiter {
	t.Helper()
	replicas := make([]*DistributedTenantRateLimiter, count)
	var recorder contract.RuntimeMetrics
	if metrics != nil {
		recorder = metrics
	}
	for i := range replicas {
		var err error
		if replicas[i], err = NewDistributedTenantRateLimiter(store, recorder, distributedConfig()); err != nil {
			t.Fatal(err)
		}
		replicas[i].Now = now
	}
	return replicas
}

func TestDistributedLimiterRejectsInvalidConfig(t *testing.T) {
	store := &fake.CacheService{}
	cases := map[string]func(*DistributedRateLimiterConfig){
		"partial limit":         func(c *DistributedRateLimiterConfig) { c.Turn = WindowLimit{Limit: 1} },
		"negative limit":        func(c *DistributedRateLimiterConfig) { c.Tool = WindowLimit{Limit: -1, Window: time.Second} },
		"empty prefix":          func(c *DistributedRateLimiterConfig) { c.KeyPrefix = "" },
		"zero store timeout":    func(c *DistributedRateLimiterConfig) { c.StoreTimeout = 0 },
		"zero recovery":         func(c *DistributedRateLimiterConfig) { c.RecoveryProbe = 0 },
		"zero fraction":         func(c *DistributedRateLimiterConfig) { c.FallbackFraction = 0 },
		"fraction above one":    func(c *DistributedRateLimiterConfig) { c.FallbackFraction = 1.5 },
		"zero fallback tenants": func(c *DistributedRateLimiterConfig) { c.FallbackMaxTenants = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := distributedConfig()
			mutate(&config)
			if _, err := NewDistributedTenantRateLimiter(store, nil, config); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	if _, err := NewDistributedTenantRateLimiter(nil, nil, distributedConfig()); err == nil {
		t.Fatal("nil store must fail")
	}
}

func TestDistributedLimiterReplicasShareTenantQuota(t *testing.T) {
	store := &fake.CacheService{}
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	replicas := newReplicas(t, store, nil, 3, func() time.Time { return now })
	tenant := domain.TenantContext{TenantID: 7}

	admitted, rejected := 0, 0
	for i := range 9 {
		err := replicas[i%3].AllowTurn(context.Background(), tenant)
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, domain.ErrRateLimited):
			rejected++
		default:
			t.Fatal(err)
		}
	}
	if admitted != 4 || rejected != 5 {
		t.Fatalf("admitted %d rejected %d, want 4/5 across replicas", admitted, rejected)
	}
	// The next window admits again.
	now = now.Add(time.Second)
	if err := replicas[0].AllowTurn(context.Background(), tenant); err != nil {
		t.Fatalf("new window = %v", err)
	}
}

func TestDistributedLimiterFleetRejectionCompensatesTenant(t *testing.T) {
	store := &fake.CacheService{}
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	replicas := newReplicas(t, store, nil, 2, func() time.Time { return now })
	tenants := []domain.TenantContext{{TenantID: 1}, {TenantID: 2}}

	// Fleet limit 6, tenant limit 4: tenants alternate so the fleet trips first.
	admitted := 0
	var lastErr error
	for i := range 8 {
		if err := replicas[i%2].AllowTurn(context.Background(), tenants[i%2]); err == nil {
			admitted++
		} else {
			lastErr = err
		}
	}
	if admitted != 6 || !errors.Is(lastErr, domain.ErrRateLimited) {
		t.Fatalf("admitted %d last %v", admitted, lastErr)
	}
	tenantKey, _, _ := windowKey("scout:rl", "turn", "t", 1, time.Second, now)
	if got := store.Count(tenantKey); got != 3 {
		t.Fatalf("tenant 1 counter = %d, want 3 after compensation", got)
	}
}

func TestDistributedLimiterFallsBackWithReducedQuotaAndRecovers(t *testing.T) {
	store := &fake.CacheService{}
	metrics := &dependencyRecorder{}
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	replicas := newReplicas(t, store, metrics, 2, func() time.Time { return now })
	tenant := domain.TenantContext{TenantID: 3}

	store.SetFail(errors.New("store down"))
	// Fraction 0.5 of limit 4: two local admissions per replica, then rejection.
	for i, replica := range replicas {
		for range 2 {
			if err := replica.AllowTurn(context.Background(), tenant); err != nil {
				t.Fatalf("replica %d fallback admit = %v", i, err)
			}
		}
		if err := replica.AllowTurn(context.Background(), tenant); !errors.Is(err, domain.ErrRateLimited) {
			t.Fatalf("replica %d fallback reject = %v", i, err)
		}
		if !replica.Degraded() {
			t.Fatalf("replica %d should be degraded", i)
		}
	}
	metrics.mu.Lock()
	dependencyErrors, observations := len(metrics.errors), len(metrics.observations)
	metrics.mu.Unlock()
	if dependencyErrors != 2 || observations != 2 || metrics.observations[0].Outcome != domain.OutcomeDegraded {
		t.Fatalf("metrics errors=%d observations=%d", dependencyErrors, observations)
	}

	// Store back but the probe window has not elapsed: still local.
	store.SetFail(nil)
	if err := replicas[0].AllowTurn(context.Background(), tenant); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("before probe = %v", err)
	}
	now = now.Add(time.Second)
	if err := replicas[0].AllowTurn(context.Background(), tenant); err != nil {
		t.Fatalf("probe = %v", err)
	}
	if replicas[0].Degraded() {
		t.Fatal("probe success must clear degraded mode")
	}
	metrics.mu.Lock()
	last := metrics.observations[len(metrics.observations)-1]
	metrics.mu.Unlock()
	if last.Outcome != domain.OutcomeOK {
		t.Fatalf("recovery observation = %+v", last)
	}
}

func TestDistributedLimiterStoreTimeoutDegrades(t *testing.T) {
	store := &fake.CacheService{}
	gate := make(chan struct{})
	store.SetBlock(gate)
	defer close(gate)
	replica := newReplicas(t, store, nil, 1, nil)[0]
	if err := replica.AllowTurn(context.Background(), domain.TenantContext{TenantID: 1}); err != nil {
		t.Fatalf("timeout must fall back locally: %v", err)
	}
	if !replica.Degraded() {
		t.Fatal("store timeout must degrade")
	}
}

func TestDistributedLimiterCallerCancellationIsNotAnOutage(t *testing.T) {
	store := &fake.CacheService{}
	gate := make(chan struct{})
	store.SetBlock(gate)
	defer close(gate)
	replica := newReplicas(t, store, nil, 1, nil)[0]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := replica.AllowTurn(ctx, domain.TenantContext{TenantID: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	if replica.Degraded() {
		t.Fatal("caller cancellation must not degrade")
	}
}

func TestDistributedLimiterCoalescesHotKey(t *testing.T) {
	store := &fake.CacheService{}
	gate := make(chan struct{})
	store.SetBlock(gate)
	counter := &sharedCounter{store: store, timeout: time.Second}
	const key = "hot"

	results := make(chan int64, 6)
	var wg sync.WaitGroup
	wg.Add(6)
	for range 6 {
		go func() {
			defer wg.Done()
			count, err := counter.increment(context.Background(), key, time.Second)
			if err != nil {
				t.Error(err)
			}
			results <- count
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for counter.pending(key) < 5 {
		if time.Now().After(deadline) {
			t.Fatal("followers did not join the pending batch")
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()
	close(results)
	seen := map[int64]bool{}
	for count := range results {
		seen[count] = true
	}
	if len(seen) != 6 || !seen[1] || !seen[6] {
		t.Fatalf("counts = %v", seen)
	}
	calls := store.Calls()
	if len(calls) != 2 || calls[0].N != 1 || calls[1].N != 5 {
		t.Fatalf("calls = %+v, want one leader and one coalesced batch", calls)
	}
}

func TestDistributedLimiterCloseIsIdempotent(t *testing.T) {
	replica := newReplicas(t, &fake.CacheService{}, nil, 1, nil)[0]
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if err := replica.AllowTurn(context.Background(), domain.TenantContext{TenantID: 1}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("closed = %v", err)
	}
}
