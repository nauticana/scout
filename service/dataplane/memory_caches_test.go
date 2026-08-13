package dataplane

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func TestMemorySessionCacheRoundTrip(t *testing.T) {
	cache := &MemorySessionCache{Capacity: 8}
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
	cache := &MemoryGraphCache{Capacity: 8}
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
	sessions := &MemorySessionCache{Capacity: 1}
	defer sessions.Close()
	if err := sessions.Put(context.Background(), 1, domain.SessionSnapshot{ConversationID: "c"}); err != nil {
		t.Fatal(err)
	}
	graphs := &MemoryGraphCache{Capacity: 1}
	defer graphs.Close()
	if err := graphs.Put(context.Background(), 1, domain.ExecutionGraph{AgentID: "a", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sessions.Get(context.Background(), 0, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
}

func TestDefinitionResolverCoalescesConcurrentMisses(t *testing.T) {
	var loads atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	graph := domain.ExecutionGraph{AgentID: "a", Version: "1"}
	resolver := &DefinitionResolver{
		Repository: &fake.ExecutionGraphRepository{GetFunc: func(context.Context, int64, string, string) (domain.ExecutionGraph, error) {
			if loads.Add(1) == 1 {
				close(started)
			}
			<-release
			return graph, nil
		}},
		Cache:   &MemoryGraphCache{Capacity: 8},
		Metrics: &fake.RuntimeMetrics{},
	}

	results := make(chan error, 4)
	resolve := func() {
		_, err := resolver.Resolve(context.Background(), 1, "a", "1")
		results <- err
	}
	go resolve()
	<-started
	for range 3 {
		go resolve()
	}
	close(release)
	for range 4 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if loads.Load() != 1 {
		t.Fatalf("repository loads = %d", loads.Load())
	}
}
