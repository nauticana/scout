package knowledge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
	"github.com/nauticana/scout/internal/stage"
)

func legs(items ...contract.KnowledgeRetriever) []contract.KnowledgeRetriever { return items }

type retrieverFunc func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error)

func (f retrieverFunc) Retrieve(ctx context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
	return f(ctx, query)
}

type rerankerFunc func(context.Context, domain.KnowledgeQuery, []domain.KnowledgeMatch) ([]domain.KnowledgeMatch, error)

func (f rerankerFunc) Rerank(ctx context.Context, query domain.KnowledgeQuery, matches []domain.KnowledgeMatch) ([]domain.KnowledgeMatch, error) {
	return f(ctx, query, matches)
}

func matches(ids ...string) []domain.KnowledgeMatch {
	result := make([]domain.KnowledgeMatch, len(ids))
	for i, id := range ids {
		result[i] = domain.KnowledgeMatch{DocumentID: id, ChunkNo: 1}
	}
	return result
}

var hybridQuery = domain.KnowledgeQuery{
	TenantContext: domain.TenantContext{TenantID: 1},
	TopK:          2,
}

func TestHybridRetrieverFusion(t *testing.T) {
	vector := retrieverFunc(func(_ context.Context, query domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		if query.TopK != 6 {
			t.Errorf("leg TopK = %d, want overfetch 6", query.TopK)
		}
		return domain.KnowledgeResult{Matches: matches("a", "b", "c"), Usage: domain.Usage{InputTokens: 5}}, nil
	})
	keyword := retrieverFunc(func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{Matches: matches("b", "d"), Usage: domain.Usage{InputTokens: 3}}, nil
	})
	retriever := &HybridRetriever{Legs: legs(vector, keyword)}
	result, err := retriever.Retrieve(context.Background(), hybridQuery)
	if err != nil {
		t.Fatal(err)
	}
	// "b" appears in both legs and outranks either single-leg head.
	if len(result.Matches) != 2 || result.Matches[0].DocumentID != "b" || result.Matches[1].DocumentID != "a" {
		t.Fatalf("fused = %+v", result.Matches)
	}
	if result.Usage.InputTokens != 8 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestHybridRetrieverSurvivesPartialLegFailure(t *testing.T) {
	failing := retrieverFunc(func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{}, errors.New("index down")
	})
	healthy := retrieverFunc(func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{Matches: matches("a")}, nil
	})
	retriever := &HybridRetriever{Legs: legs(failing, healthy)}
	result, err := retriever.Retrieve(context.Background(), hybridQuery)
	if err != nil || len(result.Matches) != 1 {
		t.Fatalf("partial = %+v, %v", result.Matches, err)
	}

	allDown := &HybridRetriever{Legs: legs(failing, failing)}
	_, err = allDown.Retrieve(context.Background(), hybridQuery)
	var stageErr *stage.Error
	if !errors.As(err, &stageErr) || stageErr.Stage != domain.StageRetrieval {
		t.Fatalf("total failure = %v", err)
	}
}

func TestHybridRetrieverRerankAndBudgetSkip(t *testing.T) {
	leg := retrieverFunc(func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{Matches: matches("a", "b")}, nil
	})
	reversed := rerankerFunc(func(_ context.Context, _ domain.KnowledgeQuery, fused []domain.KnowledgeMatch) ([]domain.KnowledgeMatch, error) {
		out := make([]domain.KnowledgeMatch, 0, len(fused))
		for i := len(fused) - 1; i >= 0; i-- {
			out = append(out, fused[i])
		}
		return out, nil
	})
	retriever := &HybridRetriever{Legs: legs(leg), Reranker: reversed}
	result, err := retriever.Retrieve(context.Background(), hybridQuery)
	if err != nil || result.Matches[0].DocumentID != "b" {
		t.Fatalf("reranked = %+v, %v", result.Matches, err)
	}

	// With less budget than MinRerankBudget remaining, the reranker is skipped.
	skipping := &HybridRetriever{Legs: legs(leg), Reranker: reversed, MinRerankBudget: time.Hour}
	query := hybridQuery
	query.Budget = 50 * time.Millisecond
	result, err = skipping.Retrieve(context.Background(), query)
	if err != nil || result.Matches[0].DocumentID != "a" {
		t.Fatalf("skipped = %+v, %v", result.Matches, err)
	}
}

func TestHybridRetrieverRerankFailureFallsBackToFusion(t *testing.T) {
	leg := retrieverFunc(func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{Matches: matches("a", "b")}, nil
	})
	broken := rerankerFunc(func(context.Context, domain.KnowledgeQuery, []domain.KnowledgeMatch) ([]domain.KnowledgeMatch, error) {
		return nil, errors.New("reranker down")
	})
	retriever := &HybridRetriever{Legs: legs(leg), Reranker: broken}
	result, err := retriever.Retrieve(context.Background(), hybridQuery)
	if err != nil || result.Matches[0].DocumentID != "a" {
		t.Fatalf("fallback = %+v, %v", result.Matches, err)
	}
	if len(result.Degradations) != 1 || result.Degradations[0] != domain.KnowledgeDegradationRerankerFailed {
		t.Fatalf("degradations = %v", result.Degradations)
	}
}

func TestHybridRetrieverRejectsUnknownRerankCandidate(t *testing.T) {
	leg := retrieverFunc(func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{Matches: matches("authorized")}, nil
	})
	reranker := rerankerFunc(func(context.Context, domain.KnowledgeQuery, []domain.KnowledgeMatch) ([]domain.KnowledgeMatch, error) {
		return matches("unknown"), nil
	})
	_, err := (&HybridRetriever{Legs: legs(leg), Reranker: reranker}).Retrieve(context.Background(), hybridQuery)
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown candidate = %v", err)
	}
}

func TestHybridRetrieverObservesRetrievalStage(t *testing.T) {
	var observed []domain.Observation
	observer := &fake.ObservationRecorder{RecordObservationFunc: func(_ context.Context, o domain.Observation) { observed = append(observed, o) }}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	clock := func() time.Time {
		calls++
		return start.Add(time.Duration(calls-1) * 15 * time.Millisecond)
	}
	healthy := retrieverFunc(func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{Matches: matches("a"), Usage: domain.Usage{InputTokens: 4}}, nil
	})
	failing := retrieverFunc(func(context.Context, domain.KnowledgeQuery) (domain.KnowledgeResult, error) {
		return domain.KnowledgeResult{}, errors.New("index down")
	})
	query := hybridQuery
	query.KnowledgeVersion = "kb-7"
	query.TenantContext.Tier = "gold"

	retriever := &HybridRetriever{Legs: legs(healthy, failing), Observer: observer, Now: clock}
	if _, err := retriever.Retrieve(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 1 {
		t.Fatalf("observed = %d", len(observed))
	}
	got := observed[0]
	if got.Stage != domain.StageRetrieval || got.Component != HybridRetrieverComponent || got.Outcome != domain.OutcomeDegraded || got.ErrorClass != "" {
		t.Fatalf("degraded observation = %+v", got)
	}
	if got.TenantID != 1 || got.TenantTier != "gold" || got.Versions.Knowledge != "kb-7" || got.Usage.InputTokens != 4 || got.Duration != 15*time.Millisecond {
		t.Fatalf("attribution = %+v", got)
	}

	observed = nil
	healthyOnly := &HybridRetriever{Legs: legs(healthy), Observer: observer, Now: clock}
	healthyOnly.Retrieve(context.Background(), query)
	if len(observed) != 1 || observed[0].Outcome != domain.OutcomeOK {
		t.Fatalf("ok observation = %+v", observed)
	}

	observed = nil
	allDown := &HybridRetriever{Legs: legs(failing), Observer: observer, Now: clock}
	if _, err := allDown.Retrieve(context.Background(), query); err == nil {
		t.Fatal("expected total failure")
	}
	if len(observed) != 1 || observed[0].Outcome != domain.OutcomeError || observed[0].ErrorClass != stage.ErrorClassInternal {
		t.Fatalf("error observation = %+v", observed)
	}
}
