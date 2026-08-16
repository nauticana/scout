package knowledge

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

// ShardedRetrieverConfig bounds fan-out over index shards.
type ShardedRetrieverConfig struct {
	// MaxConcurrency bounds how many shards are queried at once.
	MaxConcurrency int
}

// ShardedRetriever queries every shard of a knowledge version with the same
// authorized query, then k-way merges the sorted shard results and stops as
// soon as TopK unique chunks are known. A shard failure degrades recall; only
// a total failure fails the query. It owns no long-lived goroutines.
type ShardedRetriever struct {
	shards []contract.KnowledgeRetriever
	config ShardedRetrieverConfig
}

var _ contract.KnowledgeRetriever = (*ShardedRetriever)(nil)

// NewShardedRetriever validates the shards and configuration.
func NewShardedRetriever(shards []contract.KnowledgeRetriever, config ShardedRetrieverConfig) (*ShardedRetriever, error) {
	if len(shards) == 0 {
		return nil, fmt.Errorf("sharded retriever: at least one shard is required")
	}
	for _, shard := range shards {
		if shard == nil {
			return nil, fmt.Errorf("sharded retriever: shards cannot be nil")
		}
	}
	if config.MaxConcurrency <= 0 {
		return nil, fmt.Errorf("sharded retriever: max concurrency must be positive")
	}
	return &ShardedRetriever{shards: append([]contract.KnowledgeRetriever(nil), shards...), config: config}, nil
}

// Retrieve fans out under the query budget and merges the shard rankings.
func (retriever *ShardedRetriever) Retrieve(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	if query.TenantContext.TenantID <= 0 || query.TopK <= 0 {
		return domain.KnowledgeResult{}, fmt.Errorf("%w: tenant and positive TopK are required", domain.ErrValidation)
	}
	ctx, cancel := budgetContext(ctx, query)
	defer cancel()

	results := make([]domain.KnowledgeResult, len(retriever.shards))
	failures := make([]error, len(retriever.shards))
	semaphore := make(chan struct{}, retriever.config.MaxConcurrency)
	var wg sync.WaitGroup
	for i, shard := range retriever.shards {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			failures[i] = ctx.Err()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			results[i], failures[i] = shard.Retrieve(ctx, query)
		}()
	}
	wg.Wait()

	streams := make([][]domain.KnowledgeMatch, 0, len(results))
	var usage domain.Usage
	succeeded := 0
	for i, result := range results {
		if failures[i] != nil {
			continue
		}
		succeeded++
		var err error
		if usage, err = mergeUsage(usage, result.Usage); err != nil {
			return domain.KnowledgeResult{}, stage.At(domain.StageRetrieval, err)
		}
		streams = append(streams, result.Matches)
	}
	if succeeded == 0 {
		return domain.KnowledgeResult{}, stage.At(domain.StageRetrieval, errors.Join(failures...))
	}
	merged, err := MergeTopK(streams, query.TopK)
	if err != nil {
		return domain.KnowledgeResult{}, stage.At(domain.StageRetrieval, err)
	}
	var degradations []string
	if succeeded < len(retriever.shards) {
		degradations = append(degradations, domain.KnowledgeDegradationPartialRetrieval)
	}
	return domain.KnowledgeResult{Matches: merged, Usage: usage, Degradations: degradations}, nil
}

// MergeTopK k-way merges score-descending streams into the global top k,
// de-duplicating by chunk on first sight (correct because the first copy is
// the highest scored) and stopping once k unique chunks are known, so it
// touches O(k log n) heads and never reads the tails. Score order is checked
// lazily on each advance; a shard that violates it is rejected.
func MergeTopK(streams [][]domain.KnowledgeMatch, k int) ([]domain.KnowledgeMatch, error) {
	if k <= 0 {
		return nil, fmt.Errorf("%w: k must be positive", domain.ErrValidation)
	}
	heads := &matchHeap{}
	for i, stream := range streams {
		if len(stream) > 0 {
			heap.Push(heads, matchHead{match: stream[0], stream: i})
		}
	}
	type chunkKey struct {
		documentID string
		chunkNo    int
	}
	seen := make(map[chunkKey]struct{}, k)
	merged := make([]domain.KnowledgeMatch, 0, k)
	for heads.Len() > 0 && len(merged) < k {
		head := heap.Pop(heads).(matchHead)
		key := chunkKey{head.match.DocumentID, head.match.ChunkNo}
		if _, duplicate := seen[key]; !duplicate {
			seen[key] = struct{}{}
			merged = append(merged, head.match)
		}
		if next := head.pos + 1; next < len(streams[head.stream]) {
			candidate := streams[head.stream][next]
			if candidate.Score > head.match.Score {
				return nil, fmt.Errorf("%w: shard %d returned matches out of score order", domain.ErrValidation, head.stream)
			}
			heap.Push(heads, matchHead{match: candidate, stream: head.stream, pos: next})
		}
	}
	return merged, nil
}

type matchHead struct {
	match  domain.KnowledgeMatch
	stream int
	pos    int
}

// matchHeap orders heads by score, then chunk identity, then shard, for a deterministic merge.
type matchHeap []matchHead

func (h matchHeap) Len() int { return len(h) }
func (h matchHeap) Less(i, j int) bool {
	a, b := h[i].match, h[j].match
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.DocumentID != b.DocumentID {
		return a.DocumentID < b.DocumentID
	}
	if a.ChunkNo != b.ChunkNo {
		return a.ChunkNo < b.ChunkNo
	}
	return h[i].stream < h[j].stream
}
func (h matchHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *matchHeap) Push(x any)   { *h = append(*h, x.(matchHead)) }
func (h *matchHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func mergeUsage(total, add domain.Usage) (domain.Usage, error) {
	if add.InputTokens < 0 || add.OutputTokens < 0 || add.ToolCalls < 0 || add.CostMinorUnits < 0 {
		return domain.Usage{}, fmt.Errorf("%w: retrieval usage cannot be negative", domain.ErrValidation)
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	const maxInt = int(^uint(0) >> 1)
	if total.InputTokens > maxInt64-add.InputTokens || total.OutputTokens > maxInt64-add.OutputTokens ||
		total.ToolCalls > maxInt-add.ToolCalls || total.CostMinorUnits > maxInt64-add.CostMinorUnits {
		return domain.Usage{}, fmt.Errorf("%w: retrieval usage overflow", domain.ErrValidation)
	}
	total.InputTokens += add.InputTokens
	total.OutputTokens += add.OutputTokens
	total.ToolCalls += add.ToolCalls
	total.CostMinorUnits += add.CostMinorUnits
	if add.CostMinorUnits > 0 {
		if len(add.Currency) != 3 {
			return domain.Usage{}, fmt.Errorf("%w: retrieval cost requires a currency", domain.ErrValidation)
		}
		if total.Currency == "" {
			total.Currency = add.Currency
		} else if total.Currency != add.Currency {
			return domain.Usage{}, fmt.Errorf("%w: shards returned different currencies", domain.ErrValidation)
		}
	}
	return total, nil
}
