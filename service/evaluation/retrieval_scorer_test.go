package evaluation

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func matches(ids ...string) []domain.KnowledgeMatch {
	found := make([]domain.KnowledgeMatch, len(ids))
	for i, id := range ids {
		found[i] = domain.KnowledgeMatch{DocumentID: id, ChunkID: id + "-c1", Score: float64(len(ids) - i)}
	}
	return found
}

func authorized(count int) []bool {
	flags := make([]bool, count)
	for i := range flags {
		flags[i] = true
	}
	return flags
}

func goldenObservation(expected []string, ranked []string) domain.RetrievalObservation {
	return domain.RetrievalObservation{
		Query:          domain.GoldenQuery{TenantID: 7, QueryID: "q1", Principal: "agent@tenant", ExpectedDocumentIDs: expected},
		Matches:        matches(ranked...),
		Authorized:     authorized(len(ranked)),
		CandidateCount: 10,
	}
}

func TestRetrievalScorerComputesRankingMetricsOnFixedData(t *testing.T) {
	scorer := &RetrievalScorer{Revision: "ret-1", K: 3}
	// Expected d2 and d4; ranking is d1, d2, d3, d4 so only d2 falls inside K=3.
	observation := goldenObservation([]string{"d2", "d4"}, []string{"d1", "d2", "d3", "d4"})
	metrics, results, err := scorer.Evaluate(context.Background(), "manifest", []domain.RetrievalObservation{observation})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(metrics.RecallAtK-0.5) > 1e-9 {
		t.Fatalf("recall@3 = %f", metrics.RecallAtK)
	}
	if math.Abs(metrics.MRR-0.5) > 1e-9 {
		t.Fatalf("mrr = %f", metrics.MRR)
	}
	// nDCG@3: gain 1/log2(3) at rank 2, ideal 1/log2(2) + 1/log2(3).
	wantNDCG := (1 / math.Log2(3)) / (1/math.Log2(2) + 1/math.Log2(3))
	if math.Abs(metrics.NDCG-wantNDCG) > 1e-9 {
		t.Fatalf("ndcg = %f, want %f", metrics.NDCG, wantNDCG)
	}
	if math.Abs(metrics.FilterSelectivity-0.4) > 1e-9 {
		t.Fatalf("selectivity = %f", metrics.FilterSelectivity)
	}
	if len(results) != 1 || results[0].Role != domain.RoleCandidate || results[0].ExampleID != "q1" {
		t.Fatalf("results = %+v", results)
	}
}

func TestRetrievalScorerPerfectRankingScoresOne(t *testing.T) {
	scorer := &RetrievalScorer{Revision: "ret-1", K: 3}
	metrics, _, err := scorer.Evaluate(context.Background(), "m", []domain.RetrievalObservation{goldenObservation([]string{"d1", "d2"}, []string{"d1", "d2", "d3"})})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.RecallAtK != 1 || metrics.MRR != 1 || math.Abs(metrics.NDCG-1) > 1e-9 {
		t.Fatalf("perfect ranking = %+v", metrics)
	}
}

func TestRetrievalScorerFlagsAuthorizationLeaksAsCritical(t *testing.T) {
	scorer := &RetrievalScorer{Revision: "ret-1", K: 5}
	observation := goldenObservation([]string{"d1"}, []string{"d1", "leaked"})
	observation.Authorized[1] = false

	metrics, results, err := scorer.Evaluate(context.Background(), "m", []domain.RetrievalObservation{observation})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.AuthorizationLeaks != 1 {
		t.Fatalf("leaks = %d", metrics.AuthorizationLeaks)
	}
	var leak domain.EvaluationScore
	for _, score := range results[0].Scores {
		if score.Metric == domain.MetricAuthorizationLeaks {
			leak = score
		}
	}
	if leak.Value != 1 || !leak.Critical || !results[0].NeedsHumanReview {
		t.Fatalf("leak score = %+v, result = %+v", leak, results[0])
	}
}

func TestRetrievalScorerScoresCitationAbstentionAndFreshness(t *testing.T) {
	scorer := &RetrievalScorer{Revision: "ret-1", K: 3, MaxIngestionLag: time.Minute, MaxTombstoneLag: time.Minute}
	observation := goldenObservation([]string{"d1"}, []string{"d1", "d2"})
	observation.CitedDocumentIDs = []string{"d1", "d9"}
	observation.Query.ExpectAbstention = true
	observation.Abstained = false
	observation.IngestionLag = 30 * time.Second
	observation.TombstoneLag = 5 * time.Minute

	metrics, results, err := scorer.Evaluate(context.Background(), "m", []domain.RetrievalObservation{observation})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(metrics.CitationPrecision-0.5) > 1e-9 || metrics.AbstentionQuality != 0 {
		t.Fatalf("citation/abstention = %+v", metrics)
	}
	if metrics.IngestionLag != 30*time.Second || metrics.TombstoneLag != 5*time.Minute {
		t.Fatalf("lags = %+v", metrics)
	}
	byMetric := scoreByMetric(results[0].Scores)
	if byMetric[domain.MetricIngestionLagMs].Value != 1 {
		t.Fatalf("ingestion lag score = %+v", byMetric[domain.MetricIngestionLagMs])
	}
	if byMetric[domain.MetricTombstoneLagMs].Value != 0 || !byMetric[domain.MetricTombstoneLagMs].Critical {
		t.Fatalf("tombstone lag score = %+v", byMetric[domain.MetricTombstoneLagMs])
	}
}

func TestRetrievalScorerValidation(t *testing.T) {
	ctx := context.Background()
	if _, _, err := (&RetrievalScorer{Revision: "r"}).Evaluate(ctx, "m", []domain.RetrievalObservation{goldenObservation(nil, nil)}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero K = %v", err)
	}
	if _, _, err := (&RetrievalScorer{Revision: "r", K: 3}).Evaluate(ctx, "m", nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no observations = %v", err)
	}
	mismatched := goldenObservation([]string{"d1"}, []string{"d1", "d2"})
	mismatched.Authorized = []bool{true}
	if _, _, err := (&RetrievalScorer{Revision: "r", K: 3}).Evaluate(ctx, "m", []domain.RetrievalObservation{mismatched}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("mismatched authorization flags = %v", err)
	}
}
