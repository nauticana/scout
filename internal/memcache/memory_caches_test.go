package memcache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestMemorySessionCacheRoundTrip(t *testing.T) {
	cache := &MemorySessionCache{Capacity: 8, TTL: time.Minute}
	defer cache.Close()
	ctx := context.Background()
	snapshot := domain.SessionSnapshot{ConversationID: "c", Revision: 2}

	if _, found, err := cache.Get(ctx, 1, "c"); err != nil || found {
		t.Fatalf("empty get = %v, %v", found, err)
	}
	if err := cache.Put(ctx, 1, snapshot); err != nil {
		t.Fatal(err)
	}
	got, found, err := cache.Get(ctx, 1, "c")
	if err != nil || !found || got.Revision != 2 {
		t.Fatalf("get = %+v, %v, %v", got, found, err)
	}
	// Tenant isolation: same conversation id under another tenant misses.
	if _, found, _ := cache.Get(ctx, 2, "c"); found {
		t.Fatal("cross-tenant hit")
	}
	if err := cache.Invalidate(ctx, 1, "c"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := cache.Get(ctx, 1, "c"); found {
		t.Fatal("invalidate failed")
	}
}

func TestMemoryGraphCacheRoundTrip(t *testing.T) {
	cache := &MemoryGraphCache{Capacity: 8, TTL: time.Minute}
	defer cache.Close()
	ctx := context.Background()
	graph := domain.ExecutionGraph{AgentID: "a", Version: "1", EntryStepID: "s"}

	if err := cache.Put(ctx, 1, graph); err != nil {
		t.Fatal(err)
	}
	got, found, err := cache.Get(ctx, 1, "a", "1")
	if err != nil || !found || got.EntryStepID != "s" {
		t.Fatalf("get = %+v, %v, %v", got, found, err)
	}
	if err := cache.Invalidate(ctx, 1, "a", "1"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := cache.Get(ctx, 1, "a", "1"); found {
		t.Fatal("invalidate failed")
	}
}

func TestMemoryCachesAcceptSingleEntryCapacity(t *testing.T) {
	sessions := &MemorySessionCache{Capacity: 1, TTL: time.Minute}
	defer sessions.Close()
	if err := sessions.Put(context.Background(), 1, domain.SessionSnapshot{ConversationID: "c"}); err != nil {
		t.Fatal(err)
	}
	graphs := &MemoryGraphCache{Capacity: 1, TTL: time.Minute}
	defer graphs.Close()
	if err := graphs.Put(context.Background(), 1, domain.ExecutionGraph{AgentID: "a", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sessions.Get(context.Background(), 0, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
}

func TestMemorySessionCacheKeepsNewerRevision(t *testing.T) {
	cache := &MemorySessionCache{Capacity: 8, TTL: time.Minute}
	defer cache.Close()
	ctx := context.Background()
	if err := cache.Put(ctx, 1, domain.SessionSnapshot{ConversationID: "c", Revision: 7}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, 1, domain.SessionSnapshot{ConversationID: "c", Revision: 6}); err != nil {
		t.Fatal(err)
	}
	if got, found, _ := cache.Get(ctx, 1, "c"); !found || got.Revision != 7 {
		t.Fatalf("older put replaced newer: %+v, %v", got, found)
	}
	if err := cache.Put(ctx, 1, domain.SessionSnapshot{ConversationID: "c", Revision: 8}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := cache.Get(ctx, 1, "c"); got.Revision != 8 {
		t.Fatalf("newer put ignored: %+v", got)
	}
	if err := cache.Put(ctx, 1, domain.SessionSnapshot{ConversationID: "c", Revision: -1}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative revision = %v", err)
	}
}
