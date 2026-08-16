package knowledge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

var testScope = RetrievalCacheKeyerFunc(func(context.Context, domain.KnowledgeQuery) (RetrievalCacheScope, error) {
	return RetrievalCacheScope{EmbeddingModelVersion: "embed-3", IndexGeneration: "gen-1", PolicyVersion: "policy-1"}, nil
})

type countingRetriever struct {
	calls atomic.Int32
	inner func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error)
}

func (retriever *countingRetriever) Retrieve(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	retriever.calls.Add(1)
	return retriever.inner(ctx, query)
}

func newCached(t *testing.T, inner *countingRetriever, keyer RetrievalCacheKeyer, now func() time.Time) *CachedRetriever {
	t.Helper()
	cached, err := NewCachedRetriever(inner, keyer, CachedRetrieverConfig{Capacity: 8, TTL: time.Minute, LoadTimeout: time.Second, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return cached
}

func staticResult(ids ...string) *countingRetriever {
	return &countingRetriever{inner: func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		result := domain.KnowledgeResult{}
		for _, id := range ids {
			result.Matches = append(result.Matches, domain.KnowledgeMatch{DocumentID: id, ChunkNo: 1, Content: []byte("content-" + id), Score: 1})
		}
		return result, nil
	}}
}

func TestNewCachedRetrieverValidation(t *testing.T) {
	inner := staticResult("a")
	cases := []struct {
		name   string
		inner  contract.KnowledgeRetriever
		keyer  RetrievalCacheKeyer
		config CachedRetrieverConfig
	}{
		{"nil inner", nil, testScope, CachedRetrieverConfig{Capacity: 1, TTL: time.Second, LoadTimeout: time.Second}},
		{"nil keyer", inner, nil, CachedRetrieverConfig{Capacity: 1, TTL: time.Second, LoadTimeout: time.Second}},
		{"zero capacity", inner, testScope, CachedRetrieverConfig{TTL: time.Second, LoadTimeout: time.Second}},
		{"zero ttl", inner, testScope, CachedRetrieverConfig{Capacity: 1, LoadTimeout: time.Second}},
		{"zero load timeout", inner, testScope, CachedRetrieverConfig{Capacity: 1, TTL: time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCachedRetriever(tc.inner, tc.keyer, tc.config); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestCachedRetrieverHitsMissesAndCopies(t *testing.T) {
	inner := staticResult("a", "b")
	cached := newCached(t, inner, testScope, nil)
	first, err := cached.Retrieve(context.Background(), entitledQuery("user:a"))
	if err != nil || len(first.Matches) != 2 || len(first.Degradations) != 0 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	first.Matches[0].DocumentID = "mutated"
	first.Matches[0].Content[0] = 'X'
	second, err := cached.Retrieve(context.Background(), entitledQuery("user:a"))
	if err != nil || second.Matches[0].DocumentID != "a" || string(second.Matches[0].Content) != "content-a" || inner.calls.Load() != 1 {
		t.Fatalf("second = %+v, calls = %d, %v", second, inner.calls.Load(), err)
	}
	// A different query text, TopK, or version is a different key.
	other := entitledQuery("user:a")
	other.Query = []byte("other question")
	if _, err := cached.Retrieve(context.Background(), other); err != nil || inner.calls.Load() != 2 {
		t.Fatalf("query miss: calls = %d, %v", inner.calls.Load(), err)
	}
	other = entitledQuery("user:a")
	other.TopK = 9
	if _, err := cached.Retrieve(context.Background(), other); err != nil || inner.calls.Load() != 3 {
		t.Fatalf("topk miss: calls = %d, %v", inner.calls.Load(), err)
	}
	other = entitledQuery("user:a")
	other.KnowledgeVersion = "v2"
	if _, err := cached.Retrieve(context.Background(), other); err != nil || inner.calls.Load() != 4 {
		t.Fatalf("version miss: calls = %d, %v", inner.calls.Load(), err)
	}
	// Same entitlements under a different principal share the entry; one label more does not.
	same := entitledQuery("user:a")
	same.Principal = "delegate"
	if _, err := cached.Retrieve(context.Background(), same); err != nil || inner.calls.Load() != 4 {
		t.Fatalf("principal hit: calls = %d, %v", inner.calls.Load(), err)
	}
	if _, err := cached.Retrieve(context.Background(), entitledQuery("user:a", "group:finance")); err != nil || inner.calls.Load() != 5 {
		t.Fatalf("entitlement miss: calls = %d, %v", inner.calls.Load(), err)
	}
}

func TestCachedRetrieverKeysOnScopeAndEmbedding(t *testing.T) {
	inner := staticResult("a")
	generation := "gen-1"
	keyer := RetrievalCacheKeyerFunc(func(context.Context, domain.KnowledgeQuery) (RetrievalCacheScope, error) {
		return RetrievalCacheScope{EmbeddingModelVersion: "embed-3", IndexGeneration: generation, PolicyVersion: "p1"}, nil
	})
	cached := newCached(t, inner, keyer, nil)
	q := entitledQuery("user:a")
	q.Query = nil
	if _, err := cached.Retrieve(context.Background(), q); err != nil || inner.calls.Load() != 1 {
		t.Fatalf("embedding-only miss: %d, %v", inner.calls.Load(), err)
	}
	if _, err := cached.Retrieve(context.Background(), q); err != nil || inner.calls.Load() != 1 {
		t.Fatalf("embedding-only hit: %d, %v", inner.calls.Load(), err)
	}
	q.Embedding = []float32{0.5, -0.25, 2}
	if _, err := cached.Retrieve(context.Background(), q); err != nil || inner.calls.Load() != 2 {
		t.Fatalf("different embedding: %d, %v", inner.calls.Load(), err)
	}
	generation = "gen-2"
	if _, err := cached.Retrieve(context.Background(), q); err != nil || inner.calls.Load() != 3 {
		t.Fatalf("index generation change: %d, %v", inner.calls.Load(), err)
	}
}

func TestCachedRetrieverBypassesWhenKeyCannotBeFormed(t *testing.T) {
	cases := []struct {
		name  string
		keyer RetrievalCacheKeyer
		edit  func(*domain.KnowledgeQuery)
	}{
		{"keyer error", RetrievalCacheKeyerFunc(func(context.Context, domain.KnowledgeQuery) (RetrievalCacheScope, error) {
			return RetrievalCacheScope{}, errors.New("unknown version")
		}), func(*domain.KnowledgeQuery) {}},
		{"blank scope", RetrievalCacheKeyerFunc(func(context.Context, domain.KnowledgeQuery) (RetrievalCacheScope, error) {
			return RetrievalCacheScope{EmbeddingModelVersion: "m"}, nil
		}), func(*domain.KnowledgeQuery) {}},
		{"no query or embedding", testScope, func(q *domain.KnowledgeQuery) { q.Query, q.Embedding = nil, nil }},
		{"stale digest", testScope, func(q *domain.KnowledgeQuery) { q.EntitlementsDigest = "stale" }},
		{"no entitlements", testScope, func(q *domain.KnowledgeQuery) { q.Entitlements = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := staticResult("a")
			cached := newCached(t, inner, tc.keyer, nil)
			q := entitledQuery("user:a")
			tc.edit(&q)
			for i := 0; i < 2; i++ {
				result, err := cached.Retrieve(context.Background(), q)
				if err != nil || len(result.Degradations) != 1 || result.Degradations[0] != domain.KnowledgeDegradationCacheBypassed {
					t.Fatalf("bypass = %+v, %v", result, err)
				}
			}
			if inner.calls.Load() != 2 {
				t.Fatalf("bypassed calls = %d", inner.calls.Load())
			}
		})
	}
}

func TestCachedRetrieverPropagatesInnerErrorsAndSkipsDegraded(t *testing.T) {
	failing := &countingRetriever{inner: func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{}, domain.ErrForbidden
	}}
	cached := newCached(t, failing, testScope, nil)
	if _, err := cached.Retrieve(context.Background(), entitledQuery("user:a")); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
	if _, err := cached.Retrieve(context.Background(), entitledQuery("user:a")); !errors.Is(err, domain.ErrForbidden) || failing.calls.Load() != 2 {
		t.Fatalf("errors must not be cached: %d, %v", failing.calls.Load(), err)
	}
	degraded := &countingRetriever{inner: func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{Degradations: []string{domain.KnowledgeDegradationPartialRetrieval}}, nil
	}}
	cached = newCached(t, degraded, testScope, nil)
	for i := 0; i < 2; i++ {
		if _, err := cached.Retrieve(context.Background(), entitledQuery("user:a")); err != nil {
			t.Fatal(err)
		}
	}
	if degraded.calls.Load() != 2 {
		t.Fatalf("degraded results were cached: %d", degraded.calls.Load())
	}
}

func TestCachedRetrieverInvalidateAndTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	inner := staticResult("a")
	cached := newCached(t, inner, testScope, func() time.Time { return now })
	q := entitledQuery("user:a")
	for i := 0; i < 2; i++ {
		if _, err := cached.Retrieve(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("calls before invalidate = %d", inner.calls.Load())
	}
	cached.Invalidate(99, "kb")
	if _, err := cached.Retrieve(context.Background(), q); err != nil || inner.calls.Load() != 1 {
		t.Fatalf("foreign invalidate evicted: %d, %v", inner.calls.Load(), err)
	}
	cached.Invalidate(7, "kb")
	if _, err := cached.Retrieve(context.Background(), q); err != nil || inner.calls.Load() != 2 {
		t.Fatalf("invalidate did not evict: %d, %v", inner.calls.Load(), err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := cached.Retrieve(context.Background(), q); err != nil || inner.calls.Load() != 3 {
		t.Fatalf("ttl did not expire: %d, %v", inner.calls.Load(), err)
	}
}

func TestCachedRetrieverCoalescesConcurrentMisses(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	inner := &countingRetriever{inner: func(ctx context.Context, _ domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return domain.KnowledgeResult{}, ctx.Err()
		}
		return domain.KnowledgeResult{Matches: []domain.KnowledgeMatch{{DocumentID: "a", ChunkNo: 1}}}, nil
	}}
	cached := newCached(t, inner, testScope, nil)
	var wg sync.WaitGroup
	results := make([]error, 6)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, results[0] = cached.Retrieve(context.Background(), entitledQuery("user:a"))
	}()
	<-started
	for i := 1; i < len(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = cached.Retrieve(context.Background(), entitledQuery("user:a"))
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("coalesced calls = %d", inner.calls.Load())
	}
}

func TestCachedRetrieverBoundsDetachedLoad(t *testing.T) {
	inner := &countingRetriever{inner: func(ctx context.Context, _ domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		<-ctx.Done()
		return domain.KnowledgeResult{}, ctx.Err()
	}}
	cached, err := NewCachedRetriever(inner, testScope, CachedRetrieverConfig{Capacity: 1, TTL: time.Minute, LoadTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cached.Retrieve(context.Background(), entitledQuery("user:a")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}
