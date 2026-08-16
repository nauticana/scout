package dataplane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/dataplane/dataplanetest"
)

// testClock is a manually advanced clock shared by a queue and its suite harness.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Unix(1_700_000_000, 0).UTC()} }

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) Advance(step time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(step)
}

func newQueueHarness(t *testing.T) dataplanetest.QueueHarness {
	t.Helper()
	clock := newTestClock()
	queue, err := NewMemoryTurnQueue(MemoryTurnQueueConfig{
		Partitions: 8, ShardsPerTenant: 2, MaxMessages: 64, MaxAttempts: 2, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dataplanetest.QueueHarness{
		Dispatcher:   queue,
		Scheduler:    queue,
		DeadLettered: func() []domain.QueueMessage { messages, _ := queue.DeadLettered(); return messages },
		Advance:      clock.Advance,
		MaxAttempts:  2,
		Lease:        time.Minute,
		Close:        func() { _ = queue.Close() },
	}
}

func TestMemoryTurnQueueConformance(t *testing.T) {
	dataplanetest.RunDispatcherSuite(t, newQueueHarness)
	dataplanetest.RunSchedulerSuite(t, newQueueHarness)
}

func TestMemoryReplyHubConformance(t *testing.T) {
	dataplanetest.RunReplySuite(t, func(t *testing.T) dataplanetest.ReplyHarness {
		hub := &MemoryReplyHub{}
		return dataplanetest.ReplyHarness{Publisher: hub, Subscriber: hub}
	})
}

func TestMemoryStepIdempotencyConformance(t *testing.T) {
	dataplanetest.RunIdempotencySuite(t, func(t *testing.T) dataplanetest.IdempotencyHarness {
		return dataplanetest.IdempotencyHarness{Store: newMemoryIdempotency()}
	})
}

func TestMemoryTurnQueueRejectsInvalidConfiguration(t *testing.T) {
	for name, config := range map[string]MemoryTurnQueueConfig{
		"no partitions":  {Partitions: 0, ShardsPerTenant: 1, MaxMessages: 1, MaxAttempts: 1},
		"shards exceed":  {Partitions: 2, ShardsPerTenant: 3, MaxMessages: 1, MaxAttempts: 1},
		"no capacity":    {Partitions: 2, ShardsPerTenant: 1, MaxMessages: 0, MaxAttempts: 1},
		"no max attempt": {Partitions: 2, ShardsPerTenant: 1, MaxMessages: 1, MaxAttempts: 0},
	} {
		if _, err := NewMemoryTurnQueue(config); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}

func TestMemoryTurnQueueBoundsAndCloses(t *testing.T) {
	queue, err := NewMemoryTurnQueue(MemoryTurnQueueConfig{Partitions: 4, ShardsPerTenant: 1, MaxMessages: 1, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := queue.Enqueue(ctx, dataplanetest.Dispatch(1, "request-1", "conversation-1", []byte("a"))); err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, dataplanetest.Dispatch(1, "request-2", "conversation-2", []byte("b"))); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("overflow = %v, want rate limited", err)
	}
	if err := queue.Close(); err != nil {
		t.Fatal(err)
	}
	if err := queue.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
	if err := queue.Enqueue(ctx, dataplanetest.Dispatch(1, "request-3", "conversation-3", []byte("c"))); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("closed enqueue = %v, want conflict", err)
	}
}

func TestMemoryTurnQueueHonorsTenantWeightsAndConcurrency(t *testing.T) {
	clock := newTestClock()
	weights := &weightPolicy{weights: map[int64]int{1: 3, 2: 1}, maxConcurrent: map[int64]int{2: 1}}
	queue, err := NewMemoryTurnQueue(MemoryTurnQueueConfig{
		Partitions: 8, ShardsPerTenant: 4, MaxMessages: 32, MaxAttempts: 3, Weights: weights, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, dispatch := range []domain.TurnDispatch{
		dataplanetest.Dispatch(1, "t1-a", "c1a", []byte("a")),
		dataplanetest.Dispatch(1, "t1-b", "c1b", []byte("b")),
		dataplanetest.Dispatch(2, "t2-a", "c2a", []byte("a")),
		dataplanetest.Dispatch(2, "t2-b", "c2b", []byte("b")),
	} {
		if err := queue.Enqueue(ctx, dispatch); err != nil {
			t.Fatal(err)
		}
	}
	var tenants []int64
	for range 3 {
		lease, err := queue.Claim(ctx, "worker", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		tenants = append(tenants, lease.Message.Dispatch.Turn.TenantContext.TenantID)
	}
	// Tenant 2 is capped at one concurrent turn, so the weighted tenant takes the rest.
	counts := map[int64]int{}
	for _, tenantID := range tenants {
		counts[tenantID]++
	}
	if counts[1] != 2 || counts[2] != 1 {
		t.Fatalf("claims per tenant = %v", counts)
	}
}

type weightPolicy struct {
	weights       map[int64]int
	maxConcurrent map[int64]int
}

func (policy *weightPolicy) SchedulingWeight(_ context.Context, tenantID int64) (int, int, error) {
	weight := policy.weights[tenantID]
	if weight < 1 {
		weight = 1
	}
	return weight, policy.maxConcurrent[tenantID], nil
}

var _ contract.TenantWeightPolicy = (*weightPolicy)(nil)

// memoryIdempotency is a test-only reference for the idempotency conformance suite.
type memoryIdempotency struct {
	mu      sync.Mutex
	entries map[string]*memoryIdempotencyEntry
}

type memoryIdempotencyEntry struct {
	status string
	result domain.StepResult
}

func newMemoryIdempotency() *memoryIdempotency {
	return &memoryIdempotency{entries: make(map[string]*memoryIdempotencyEntry)}
}

func (store *memoryIdempotency) key(tenantID int64, requestID string, step domain.ExecutionStep) string {
	return checkpointIdentity(requestID, tenantID, int(step.ExecutionStepID))
}

func (store *memoryIdempotency) Begin(_ context.Context, tenantID int64, requestID string, step domain.ExecutionStep) (domain.StepResult, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	key := store.key(tenantID, requestID, step)
	entry := store.entries[key]
	switch {
	case entry == nil:
		store.entries[key] = &memoryIdempotencyEntry{status: "claimed"}
		return domain.StepResult{}, false, nil
	case entry.status == "committed":
		return entry.result, true, nil
	case entry.status == "abandoned":
		entry.status = "claimed"
		return domain.StepResult{}, false, nil
	default:
		return domain.StepResult{}, false, fmt.Errorf("%w: step is claimed", domain.ErrConflict)
	}
}

func (store *memoryIdempotency) Commit(_ context.Context, tenantID int64, requestID string, step domain.ExecutionStep, result domain.StepResult) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry := store.entries[store.key(tenantID, requestID, step)]
	if entry == nil || entry.status != "claimed" {
		return fmt.Errorf("%w: step is not claimed", domain.ErrConflict)
	}
	entry.status, entry.result = "committed", result
	return nil
}

func (store *memoryIdempotency) Abandon(_ context.Context, tenantID int64, requestID string, step domain.ExecutionStep) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	entry := store.entries[store.key(tenantID, requestID, step)]
	if entry == nil {
		return fmt.Errorf("%w: step was never claimed", domain.ErrNotFound)
	}
	if entry.status == "committed" {
		return fmt.Errorf("%w: step is committed", domain.ErrConflict)
	}
	entry.status = "abandoned"
	return nil
}

var _ contract.StepIdempotencyStore = (*memoryIdempotency)(nil)
