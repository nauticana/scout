package dataplane

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
	"github.com/nauticana/scout/internal/memcache"
)

func TestSessionCoordinatorReturnsCacheHit(t *testing.T) {
	want := domain.SessionSnapshot{ConversationID: "conversation", Revision: 3}
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{LoadFunc: func(context.Context, int64, string) (domain.SessionSnapshot, error) {
			t.Fatal("store must not be called")
			return domain.SessionSnapshot{}, nil
		}},
		Cache: &fake.HotSessionCache{GetFunc: func(context.Context, int64, string) (domain.SessionSnapshot, bool, error) {
			return want, true, nil
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	got, err := coordinator.Load(context.Background(), 7, "conversation")
	if err != nil || got.Revision != want.Revision {
		t.Fatalf("snapshot = %+v, error = %v", got, err)
	}
}

func TestSessionCoordinatorFallsBackAndReportsCacheFailure(t *testing.T) {
	cacheErr := errors.New("cache unavailable")
	want := domain.SessionSnapshot{ConversationID: "conversation", Revision: 3}
	put := false
	var reported error
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{LoadFunc: func(context.Context, int64, string) (domain.SessionSnapshot, error) {
			return want, nil
		}},
		Cache: &fake.HotSessionCache{
			GetFunc: func(context.Context, int64, string) (domain.SessionSnapshot, bool, error) {
				return domain.SessionSnapshot{}, false, cacheErr
			},
			PutFunc: func(context.Context, int64, domain.SessionSnapshot) error {
				put = true
				return nil
			},
		},
		Metrics: &fake.RuntimeMetrics{RecordDependencyFunc: func(_ context.Context, _ int64, dependency, _ string, _ domain.Usage, err error) {
			if dependency != "session_cache" {
				t.Fatalf("dependency = %q", dependency)
			}
			reported = err
		}},
	}
	got, err := coordinator.Load(context.Background(), 7, "conversation")
	if err != nil || got.Revision != want.Revision || !put || !errors.Is(reported, cacheErr) {
		t.Fatalf("snapshot = %+v, put = %v, reported = %v, error = %v", got, put, reported, err)
	}
}

func TestSessionCoordinatorPersistsBeforeInvalidation(t *testing.T) {
	var calls []string
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{
			CheckpointFunc: func(context.Context, int64, int64, domain.StepCheckpoint) error {
				calls = append(calls, "checkpoint")
				return nil
			},
			CompleteFunc: func(context.Context, int64, string, int64, domain.TurnResult) error {
				calls = append(calls, "complete")
				return nil
			},
		},
		Cache: &fake.HotSessionCache{InvalidateFunc: func(context.Context, int64, string) error {
			calls = append(calls, "invalidate")
			return nil
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	checkpoint := domain.StepCheckpoint{ConversationID: "conversation"}
	if err := coordinator.Checkpoint(context.Background(), 7, 0, checkpoint); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if err := coordinator.Complete(context.Background(), 7, "conversation", 1, domain.TurnResult{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want := []string{"checkpoint", "invalidate", "complete", "invalidate"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestSessionCoordinatorInvalidatesAfterStoreFailure(t *testing.T) {
	want := domain.ErrRevisionConflict
	invalidated := false
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{
			CheckpointFunc: func(context.Context, int64, int64, domain.StepCheckpoint) error { return want },
			CompleteFunc:   func(context.Context, int64, string, int64, domain.TurnResult) error { return want },
		},
		Cache: &fake.HotSessionCache{InvalidateFunc: func(context.Context, int64, string) error {
			invalidated = true
			return nil
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	err := coordinator.Checkpoint(context.Background(), 7, 2, domain.StepCheckpoint{ConversationID: "conversation"})
	if !errors.Is(err, want) || !invalidated {
		t.Fatalf("checkpoint error = %v, invalidated = %v", err, invalidated)
	}
	invalidated = false
	err = coordinator.Complete(context.Background(), 7, "conversation", 2, domain.TurnResult{})
	if !errors.Is(err, want) || !invalidated {
		t.Fatalf("complete error = %v, invalidated = %v", err, invalidated)
	}
}

func TestSessionCoordinatorInvalidatesAfterWriteCancelsRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{CheckpointFunc: func(context.Context, int64, int64, domain.StepCheckpoint) error {
			cancel()
			return nil
		}},
		Cache: &fake.HotSessionCache{InvalidateFunc: func(cacheCtx context.Context, _ int64, _ string) error {
			if err := cacheCtx.Err(); err != nil {
				t.Fatalf("cache context = %v", err)
			}
			return nil
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	if err := coordinator.Checkpoint(ctx, 7, 2, domain.StepCheckpoint{ConversationID: "conversation"}); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCoordinatorSurfacesPostCommitInvalidationFailure(t *testing.T) {
	cacheErr := errors.New("cache unavailable")
	coordinator := &SessionCoordinator{
		Store: &fake.DurableSessionStore{CheckpointFunc: func(context.Context, int64, int64, domain.StepCheckpoint) error {
			return nil
		}},
		Cache: &fake.HotSessionCache{InvalidateFunc: func(context.Context, int64, string) error {
			return cacheErr
		}},
		Metrics: &fake.RuntimeMetrics{},
	}
	err := coordinator.Checkpoint(context.Background(), 7, 2, domain.StepCheckpoint{ConversationID: "conversation"})
	if !errors.Is(err, cacheErr) {
		t.Fatalf("error = %v", err)
	}
}

// durableFake is a revision-tracking DurableSessionStore for cache-discipline
// tests; the first Load blocks when loading/release are set.
type durableFake struct {
	mu       sync.Mutex
	revision int64
	once     sync.Once
	loading  chan struct{}
	release  chan struct{}
}

func (store *durableFake) Load(context.Context, int64, string) (domain.SessionSnapshot, error) {
	if store.loading != nil {
		store.once.Do(func() {
			store.loading <- struct{}{}
			<-store.release
		})
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return domain.SessionSnapshot{ConversationID: "conversation", Revision: store.revision}, nil
}

func (store *durableFake) Checkpoint(_ context.Context, _ int64, expected int64, _ domain.StepCheckpoint) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.revision != expected {
		return domain.ErrRevisionConflict
	}
	store.revision++
	return nil
}

func (store *durableFake) Complete(_ context.Context, _ int64, _ string, expected int64, _ domain.TurnResult) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.revision != expected {
		return domain.ErrRevisionConflict
	}
	return nil
}

func newCachedCoordinator(t *testing.T, store *durableFake) (*SessionCoordinator, *memcache.MemorySessionCache) {
	t.Helper()
	cache := &memcache.MemorySessionCache{Capacity: 8, TTL: time.Minute}
	t.Cleanup(func() { cache.Close() })
	return &SessionCoordinator{Store: store, Cache: cache, Metrics: &fake.RuntimeMetrics{}}, cache
}

func TestSessionCoordinatorReadAfterWriteSeesNewRevision(t *testing.T) {
	store := &durableFake{revision: 5}
	coordinator, cache := newCachedCoordinator(t, store)
	ctx := context.Background()
	if snapshot, err := coordinator.Load(ctx, 7, "conversation"); err != nil || snapshot.Revision != 5 {
		t.Fatalf("warm = %+v, %v", snapshot, err)
	}
	if err := coordinator.Checkpoint(ctx, 7, 5, domain.StepCheckpoint{ConversationID: "conversation"}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := cache.Get(ctx, 7, "conversation"); found {
		t.Fatal("write left the old revision cached")
	}
	if snapshot, err := coordinator.Load(ctx, 7, "conversation"); err != nil || snapshot.Revision != 6 {
		t.Fatalf("read after write = %+v, %v", snapshot, err)
	}
	if err := coordinator.Complete(ctx, 7, "conversation", 6, domain.TurnResult{}); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := coordinator.Load(ctx, 7, "conversation"); err != nil || snapshot.Revision != 6 {
		t.Fatalf("read after complete = %+v, %v", snapshot, err)
	}
}

func TestSessionCoordinatorConcurrentCheckpointNeverRepopulatesSupersededRead(t *testing.T) {
	store := &durableFake{revision: 5, loading: make(chan struct{}), release: make(chan struct{})}
	coordinator, cache := newCachedCoordinator(t, store)
	ctx := context.Background()

	loaded := make(chan domain.SessionSnapshot, 1)
	go func() {
		snapshot, err := coordinator.Load(ctx, 7, "conversation")
		if err != nil {
			t.Error(err)
		}
		loaded <- snapshot
	}()
	<-store.loading // the slow read has started against revision 5
	if err := coordinator.Checkpoint(ctx, 7, 5, domain.StepCheckpoint{ConversationID: "conversation"}); err != nil {
		t.Fatal(err)
	}
	close(store.release)
	stale := <-loaded
	if stale.Revision != 6 && stale.Revision != 5 {
		t.Fatalf("overlapping read = %+v", stale)
	}
	if cached, found, _ := cache.Get(ctx, 7, "conversation"); found && cached.Revision < 6 {
		t.Fatalf("superseded revision %d repopulated the cache", cached.Revision)
	}
	if snapshot, err := coordinator.Load(ctx, 7, "conversation"); err != nil || snapshot.Revision != 6 {
		t.Fatalf("read after concurrent write = %+v, %v", snapshot, err)
	}
}

func TestSessionCoordinatorRejectsStaleCachedRevision(t *testing.T) {
	store := &durableFake{revision: 6}
	coordinator, cache := newCachedCoordinator(t, store)
	ctx := context.Background()
	if err := coordinator.Checkpoint(ctx, 7, 6, domain.StepCheckpoint{ConversationID: "conversation"}); err != nil {
		t.Fatal(err)
	}
	// Another tier repopulates an older revision behind the coordinator's back.
	if err := cache.Put(ctx, 7, domain.SessionSnapshot{ConversationID: "conversation", Revision: 6}); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := coordinator.Load(ctx, 7, "conversation"); err != nil || snapshot.Revision != 7 {
		t.Fatalf("stale hit served = %+v, %v", snapshot, err)
	}
	if cached, found, _ := cache.Get(ctx, 7, "conversation"); !found || cached.Revision != 7 {
		t.Fatalf("cache after refresh = %+v, %v", cached, found)
	}
	// A durable read behind a locally written revision fails closed rather than regressing.
	store.mu.Lock()
	store.revision = 3
	store.mu.Unlock()
	_ = cache.Invalidate(ctx, 7, "conversation")
	if _, err := coordinator.Load(ctx, 7, "conversation"); !errors.Is(err, domain.ErrStaleEvidence) {
		t.Fatalf("regressed durable read = %v", err)
	}
}

func TestSessionCoordinatorRecoversFromCacheLoss(t *testing.T) {
	store := &durableFake{revision: 2}
	coordinator, cache := newCachedCoordinator(t, store)
	ctx := context.Background()
	if _, err := coordinator.Load(ctx, 7, "conversation"); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Checkpoint(ctx, 7, 2, domain.StepCheckpoint{ConversationID: "conversation"}); err != nil {
		t.Fatal(err)
	}
	// The whole cache tier is lost and replaced empty.
	_ = cache.Close()
	replacement := &memcache.MemorySessionCache{Capacity: 8, TTL: time.Minute}
	t.Cleanup(func() { replacement.Close() })
	coordinator.Cache = replacement
	if snapshot, err := coordinator.Load(ctx, 7, "conversation"); err != nil || snapshot.Revision != 3 {
		t.Fatalf("after cache loss = %+v, %v", snapshot, err)
	}
	if err := coordinator.Checkpoint(ctx, 7, 3, domain.StepCheckpoint{ConversationID: "conversation"}); err != nil {
		t.Fatal(err)
	}
	// Cache errors on every operation still leave durable reads and writes correct.
	broken := errors.New("cache down")
	coordinator.Cache = &fake.HotSessionCache{
		GetFunc: func(context.Context, int64, string) (domain.SessionSnapshot, bool, error) {
			return domain.SessionSnapshot{}, false, broken
		},
		PutFunc:        func(context.Context, int64, domain.SessionSnapshot) error { return broken },
		InvalidateFunc: func(context.Context, int64, string) error { return broken },
	}
	if snapshot, err := coordinator.Load(ctx, 7, "conversation"); err != nil || snapshot.Revision != 4 {
		t.Fatalf("with broken cache = %+v, %v", snapshot, err)
	}
	if err := coordinator.Checkpoint(ctx, 7, 4, domain.StepCheckpoint{ConversationID: "conversation"}); !errors.Is(err, broken) || store.revision != 5 {
		t.Fatalf("checkpoint with broken cache = %v, revision = %d", err, store.revision)
	}
}
