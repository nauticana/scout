package knowledge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

// HybridRetriever runs every leg concurrently under the query budget, fuses
// with reciprocal-rank fusion, then reranks only if budget remains; a late
// optional stage is skipped, never allowed to eat generation time.
type HybridRetriever struct {
	// Legs are the concurrent retrieval paths, e.g. vector and keyword.
	Legs []contract.KnowledgeRetriever
	// Reranker reorders the fused candidates; nil keeps fusion order.
	Reranker contract.KnowledgeReranker
	// Overfetch multiplies TopK per leg before fusion; default 3.
	Overfetch int
	// MinRerankBudget skips reranking when less deadline remains; default 0.
	MinRerankBudget time.Duration
}

var _ contract.KnowledgeRetriever = (*HybridRetriever)(nil)

const rrfOffset = 60

// Retrieve returns the fused, optionally reranked, top TopK matches.
func (retriever *HybridRetriever) Retrieve(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	if len(retriever.Legs) == 0 {
		return domain.KnowledgeResult{}, fmt.Errorf("hybrid retriever: at least one leg is required")
	}
	for _, leg := range retriever.Legs {
		if leg == nil {
			return domain.KnowledgeResult{}, fmt.Errorf("hybrid retriever: retrieval legs cannot be nil")
		}
	}
	if query.TenantContext.TenantID <= 0 || query.TopK <= 0 {
		return domain.KnowledgeResult{}, fmt.Errorf("%w: tenant and positive TopK are required", domain.ErrValidation)
	}
	if query.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, query.Budget)
		defer cancel()
	}
	overfetch := retriever.Overfetch
	if overfetch <= 0 {
		overfetch = 3
	}
	if query.TopK > int(^uint(0)>>1)/overfetch {
		return domain.KnowledgeResult{}, fmt.Errorf("%w: TopK overflows overfetch", domain.ErrValidation)
	}
	legQuery := query
	legQuery.TopK = query.TopK * overfetch

	results := make([]domain.KnowledgeResult, len(retriever.Legs))
	failures := make([]error, len(retriever.Legs))
	var wg sync.WaitGroup
	wg.Add(len(retriever.Legs))
	for i, leg := range retriever.Legs {
		go func() {
			defer wg.Done()
			results[i], failures[i] = leg.Retrieve(ctx, legQuery)
		}()
	}
	wg.Wait()

	// Availability over completeness: partial leg failures degrade recall,
	// only a total failure fails the query.
	fused, usage, succeeded, err := fuse(results, failures)
	if err != nil {
		return domain.KnowledgeResult{}, stage.At(domain.StageRetrieval, err)
	}
	if succeeded == 0 {
		return domain.KnowledgeResult{}, stage.At(domain.StageRetrieval, errors.Join(failures...))
	}
	var degradations []string
	if succeeded < len(retriever.Legs) {
		degradations = append(degradations, domain.KnowledgeDegradationPartialRetrieval)
	}
	if retriever.Reranker != nil && rerankBudgetLeft(ctx, retriever.MinRerankBudget) {
		reranked, err := retriever.Reranker.Rerank(ctx, query, fused)
		if err != nil {
			degradations = append(degradations, domain.KnowledgeDegradationRerankerFailed)
		} else {
			fused, err = authorizedRerank(fused, reranked)
			if err != nil {
				return domain.KnowledgeResult{}, stage.At(domain.StageRetrieval, err)
			}
		}
	}
	if len(fused) > query.TopK {
		fused = fused[:query.TopK]
	}
	return domain.KnowledgeResult{Matches: fused, Usage: usage, Degradations: degradations}, nil
}

// fuse merges leg rankings by reciprocal rank, keeping each chunk's first-seen content.
func fuse(results []domain.KnowledgeResult, failures []error) ([]domain.KnowledgeMatch, domain.Usage, int, error) {
	type chunkKey struct {
		documentID string
		chunkNo    int
	}
	scores := make(map[chunkKey]float64)
	first := make(map[chunkKey]domain.KnowledgeMatch)
	var order []chunkKey
	var usage domain.Usage
	succeeded := 0
	for i, result := range results {
		if failures[i] != nil {
			continue
		}
		succeeded++
		if result.Usage.InputTokens < 0 || result.Usage.OutputTokens < 0 || result.Usage.ToolCalls < 0 || result.Usage.CostMinorUnits < 0 {
			return nil, domain.Usage{}, 0, fmt.Errorf("%w: retrieval usage cannot be negative", domain.ErrValidation)
		}
		const maxInt64 = int64(^uint64(0) >> 1)
		const maxInt = int(^uint(0) >> 1)
		if usage.InputTokens > maxInt64-result.Usage.InputTokens || usage.OutputTokens > maxInt64-result.Usage.OutputTokens ||
			usage.ToolCalls > maxInt-result.Usage.ToolCalls || usage.CostMinorUnits > maxInt64-result.Usage.CostMinorUnits {
			return nil, domain.Usage{}, 0, fmt.Errorf("%w: retrieval usage overflow", domain.ErrValidation)
		}
		usage.InputTokens += result.Usage.InputTokens
		usage.OutputTokens += result.Usage.OutputTokens
		usage.ToolCalls += result.Usage.ToolCalls
		usage.CostMinorUnits += result.Usage.CostMinorUnits
		if result.Usage.CostMinorUnits > 0 {
			if len(result.Usage.Currency) != 3 {
				return nil, domain.Usage{}, 0, fmt.Errorf("%w: retrieval cost requires a currency", domain.ErrValidation)
			}
			if usage.Currency == "" {
				usage.Currency = result.Usage.Currency
			} else if usage.Currency != result.Usage.Currency {
				return nil, domain.Usage{}, 0, fmt.Errorf("%w: retrieval legs returned different currencies", domain.ErrValidation)
			}
		}
		for rank, match := range result.Matches {
			key := chunkKey{match.DocumentID, match.ChunkNo}
			if _, seen := scores[key]; !seen {
				first[key] = match
				order = append(order, key)
			}
			scores[key] += 1 / float64(rrfOffset+rank+1)
		}
	}
	fused := make([]domain.KnowledgeMatch, 0, len(order))
	for _, key := range order {
		match := first[key]
		match.Score = scores[key]
		fused = append(fused, match)
	}
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].Score != fused[j].Score {
			return fused[i].Score > fused[j].Score
		}
		if fused[i].DocumentID != fused[j].DocumentID {
			return fused[i].DocumentID < fused[j].DocumentID
		}
		return fused[i].ChunkNo < fused[j].ChunkNo
	})
	return fused, usage, succeeded, nil
}

func authorizedRerank(original, reranked []domain.KnowledgeMatch) ([]domain.KnowledgeMatch, error) {
	type chunkKey struct {
		documentID string
		chunkNo    int
	}
	if len(reranked) != len(original) {
		return nil, fmt.Errorf("%w: reranker changed candidate count", domain.ErrValidation)
	}
	allowed := make(map[chunkKey]domain.KnowledgeMatch, len(original))
	for _, match := range original {
		allowed[chunkKey{match.DocumentID, match.ChunkNo}] = match
	}
	result := make([]domain.KnowledgeMatch, 0, len(reranked))
	for _, match := range reranked {
		key := chunkKey{match.DocumentID, match.ChunkNo}
		authorized, ok := allowed[key]
		if !ok {
			return nil, fmt.Errorf("%w: reranker returned an unknown candidate", domain.ErrValidation)
		}
		delete(allowed, key)
		result = append(result, authorized)
	}
	return result, nil
}

func rerankBudgetLeft(ctx context.Context, minimum time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > minimum
}
