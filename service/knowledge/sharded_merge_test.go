package knowledge

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
	"github.com/nauticana/scout/internal/stage"
)

func scored(pairs ...any) []domain.KnowledgeMatch {
	result := make([]domain.KnowledgeMatch, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		result = append(result, domain.KnowledgeMatch{DocumentID: pairs[i].(string), ChunkNo: 1, Score: pairs[i+1].(float64)})
	}
	return result
}

func shardOf(matches []domain.KnowledgeMatch, usage domain.Usage) contract.KnowledgeRetriever {
	return &fake.KnowledgeRetriever{RetrieveFunc: func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{Matches: matches, Usage: usage}, nil
	}}
}

func ids(matches []domain.KnowledgeMatch) []string {
	out := make([]string, len(matches))
	for i, match := range matches {
		out[i] = match.DocumentID
	}
	return out
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMergeTopK(t *testing.T) {
	cases := []struct {
		name    string
		streams [][]domain.KnowledgeMatch
		k       int
		want    []string
		wantErr error
	}{
		{"global order across shards", [][]domain.KnowledgeMatch{scored("a", 0.9, "c", 0.5, "e", 0.1), scored("b", 0.8, "d", 0.6)}, 3, []string{"a", "b", "d"}, nil},
		{"dedupe keeps first seen", [][]domain.KnowledgeMatch{scored("a", 0.9, "b", 0.4), scored("b", 0.7, "a", 0.3, "c", 0.2)}, 3, []string{"a", "b", "c"}, nil},
		{"fewer than k", [][]domain.KnowledgeMatch{scored("a", 0.9), nil, scored("a", 0.5)}, 5, []string{"a"}, nil},
		{"tie broken by identity then shard", [][]domain.KnowledgeMatch{scored("z", 0.5), scored("m", 0.5)}, 2, []string{"m", "z"}, nil},
		{"unsorted shard rejected", [][]domain.KnowledgeMatch{scored("a", 0.1, "b", 0.9)}, 2, nil, domain.ErrValidation},
		{"zero k rejected", nil, 0, nil, domain.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			merged, err := MergeTopK(tc.streams, tc.k)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := ids(merged); !equalIDs(got, tc.want) {
				t.Fatalf("merged = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMergeTopKStopsEarly(t *testing.T) {
	// Only heads that can reach the top k are visited: an order violation deep
	// in a tail is never read, while one right after the cut is.
	long := make([]domain.KnowledgeMatch, 10_000)
	for i := range long {
		long[i] = domain.KnowledgeMatch{DocumentID: string(rune('a' + i%26)), ChunkNo: i, Score: float64(len(long) - i)}
	}
	long[len(long)-1].Score = 1e6
	merged, err := MergeTopK([][]domain.KnowledgeMatch{long, scored("zz", 1e9)}, 3)
	if err != nil || len(merged) != 3 || merged[0].DocumentID != "zz" {
		t.Fatalf("merged = %v, %v", ids(merged), err)
	}
	long[3].Score = 1e6
	if _, err := MergeTopK([][]domain.KnowledgeMatch{long}, 3); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("violation after the cut = %v", err)
	}
}

func TestNewShardedRetrieverValidation(t *testing.T) {
	healthy := shardOf(nil, domain.Usage{})
	if _, err := NewShardedRetriever(nil, ShardedRetrieverConfig{MaxConcurrency: 1}); err == nil {
		t.Fatal("no shards accepted")
	}
	if _, err := NewShardedRetriever([]contract.KnowledgeRetriever{healthy, nil}, ShardedRetrieverConfig{MaxConcurrency: 1}); err == nil {
		t.Fatal("nil shard accepted")
	}
	if _, err := NewShardedRetriever([]contract.KnowledgeRetriever{healthy}, ShardedRetrieverConfig{}); err == nil {
		t.Fatal("zero concurrency accepted")
	}
}

func TestShardedRetrieverMergesAndSumsUsage(t *testing.T) {
	retriever, err := NewShardedRetriever([]contract.KnowledgeRetriever{
		shardOf(scored("a", 0.9, "c", 0.5), domain.Usage{InputTokens: 2, CostMinorUnits: 3, Currency: "USD"}),
		shardOf(scored("b", 0.8), domain.Usage{InputTokens: 4}),
	}, ShardedRetrieverConfig{MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	q := entitledQuery("user:a")
	result, err := retriever.Retrieve(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(result.Matches); !equalIDs(got, []string{"a", "b"}) || result.Usage.InputTokens != 6 || result.Usage.CostMinorUnits != 3 || result.Usage.Currency != "USD" || len(result.Degradations) != 0 {
		t.Fatalf("result = %+v", result)
	}
	q.TopK = 0
	if _, err := retriever.Retrieve(context.Background(), q); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("validation = %v", err)
	}
}

func TestShardedRetrieverPartialAndTotalFailure(t *testing.T) {
	failing := &fake.KnowledgeRetriever{RetrieveFunc: func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{}, errors.New("shard down")
	}}
	partial, _ := NewShardedRetriever([]contract.KnowledgeRetriever{failing, shardOf(scored("a", 0.9), domain.Usage{})}, ShardedRetrieverConfig{MaxConcurrency: 2})
	result, err := partial.Retrieve(context.Background(), entitledQuery("user:a"))
	if err != nil || len(result.Matches) != 1 || len(result.Degradations) != 1 || result.Degradations[0] != domain.KnowledgeDegradationPartialRetrieval {
		t.Fatalf("partial = %+v, %v", result, err)
	}
	total, _ := NewShardedRetriever([]contract.KnowledgeRetriever{failing, failing}, ShardedRetrieverConfig{MaxConcurrency: 2})
	_, err = total.Retrieve(context.Background(), entitledQuery("user:a"))
	var stageErr *stage.Error
	if !errors.As(err, &stageErr) || stageErr.Stage != domain.StageRetrieval {
		t.Fatalf("total failure = %v", err)
	}
	unsorted, _ := NewShardedRetriever([]contract.KnowledgeRetriever{shardOf(scored("a", 0.1, "b", 0.9), domain.Usage{})}, ShardedRetrieverConfig{MaxConcurrency: 1})
	if _, err := unsorted.Retrieve(context.Background(), entitledQuery("user:a")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unsorted = %v", err)
	}
	mixed, _ := NewShardedRetriever([]contract.KnowledgeRetriever{
		shardOf(nil, domain.Usage{CostMinorUnits: 1, Currency: "USD"}), shardOf(nil, domain.Usage{CostMinorUnits: 1, Currency: "EUR"}),
	}, ShardedRetrieverConfig{MaxConcurrency: 2})
	if _, err := mixed.Retrieve(context.Background(), entitledQuery("user:a")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("mixed currency = %v", err)
	}
}

func TestShardedRetrieverBoundsConcurrency(t *testing.T) {
	var running, peak atomic.Int32
	slow := &fake.KnowledgeRetriever{RetrieveFunc: func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		current := running.Add(1)
		for {
			observed := peak.Load()
			if current <= observed || peak.CompareAndSwap(observed, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
		return domain.KnowledgeResult{}, nil
	}}
	retriever, _ := NewShardedRetriever([]contract.KnowledgeRetriever{slow, slow, slow, slow, slow, slow}, ShardedRetrieverConfig{MaxConcurrency: 2})
	if _, err := retriever.Retrieve(context.Background(), entitledQuery("user:a")); err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 2 {
		t.Fatalf("peak concurrency = %d", peak.Load())
	}
}

func TestShardedRetrieverGoroutinesExitOnCancel(t *testing.T) {
	entered := make(chan struct{}, 8)
	blocking := &fake.KnowledgeRetriever{RetrieveFunc: func(ctx context.Context, _ domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return domain.KnowledgeResult{}, ctx.Err()
	}}
	retriever, _ := NewShardedRetriever([]contract.KnowledgeRetriever{blocking, blocking, blocking, blocking}, ShardedRetrieverConfig{MaxConcurrency: 2})
	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := retriever.Retrieve(ctx, entitledQuery("user:a"))
		done <- err
	}()
	<-entered
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retrieve did not return after cancel")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("goroutines = %d, baseline %d", got, baseline)
	}
}

func TestShardedRetrieverHonorsBudget(t *testing.T) {
	blocking := &fake.KnowledgeRetriever{RetrieveFunc: func(ctx context.Context, _ domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		<-ctx.Done()
		return domain.KnowledgeResult{}, ctx.Err()
	}}
	retriever, _ := NewShardedRetriever([]contract.KnowledgeRetriever{blocking}, ShardedRetrieverConfig{MaxConcurrency: 1})
	q := entitledQuery("user:a")
	q.Budget = 10 * time.Millisecond
	if _, err := retriever.Retrieve(context.Background(), q); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}
