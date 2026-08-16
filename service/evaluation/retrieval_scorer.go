package evaluation

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// RetrievalScorer evaluates replayed golden queries independently of
// generation. Any match outside the golden principal's entitlements is a
// critical authorization leak, regardless of the ranking metrics.
type RetrievalScorer struct {
	Revision string
	// K bounds the rank cut for recall@K and nDCG@K; must be positive.
	K int
	// MaxIngestionLag and MaxTombstoneLag score freshness; zero skips those metrics.
	MaxIngestionLag time.Duration
	MaxTombstoneLag time.Duration
}

var _ contract.RetrievalEvaluator = (*RetrievalScorer)(nil)

// Version identifies this scorer revision.
func (scorer *RetrievalScorer) Version() domain.EvaluatorVersion {
	return domain.EvaluatorVersion{Kind: evaluatorKindRetrieval, Version: scorer.Revision}
}

// Evaluate returns the aggregate metrics and one candidate-role result per query.
func (scorer *RetrievalScorer) Evaluate(_ context.Context, manifestID string, observations []domain.RetrievalObservation) (domain.RetrievalMetrics, []domain.EvaluationResult, error) {
	if strings.TrimSpace(scorer.Revision) == "" || scorer.K <= 0 {
		return domain.RetrievalMetrics{}, nil, fmt.Errorf("%w: retrieval scorer needs a revision and positive K", domain.ErrValidation)
	}
	if scorer.MaxIngestionLag < 0 || scorer.MaxTombstoneLag < 0 {
		return domain.RetrievalMetrics{}, nil, fmt.Errorf("%w: freshness budgets cannot be negative", domain.ErrValidation)
	}
	if len(observations) == 0 {
		return domain.RetrievalMetrics{}, nil, fmt.Errorf("%w: at least one observation is required", domain.ErrValidation)
	}
	version := scorer.Version()
	metrics := domain.RetrievalMetrics{K: scorer.K, Samples: len(observations)}
	results := make([]domain.EvaluationResult, 0, len(observations))
	var recallSum, mrrSum, ndcgSum, selectivitySum, citationSum, abstentionSum float64
	for _, observation := range observations {
		if strings.TrimSpace(observation.Query.QueryID) == "" {
			return domain.RetrievalMetrics{}, nil, fmt.Errorf("%w: golden query id is required", domain.ErrValidation)
		}
		if len(observation.Authorized) != len(observation.Matches) {
			return domain.RetrievalMetrics{}, nil, fmt.Errorf("%w: query %q has %d matches but %d authorization flags", domain.ErrValidation, observation.Query.QueryID, len(observation.Matches), len(observation.Authorized))
		}
		leaks := 0
		for _, authorized := range observation.Authorized {
			if !authorized {
				leaks++
			}
		}
		metrics.AuthorizationLeaks += leaks

		relevant := make(map[string]struct{}, len(observation.Query.ExpectedDocumentIDs))
		for _, id := range observation.Query.ExpectedDocumentIDs {
			relevant[id] = struct{}{}
		}
		ranked := rankedDocuments(observation.Matches, scorer.K)
		recall := recallAtK(ranked, relevant)
		reciprocal := reciprocalRank(ranked, relevant)
		gain := ndcgAtK(ranked, relevant)
		selectivity := filterSelectivity(observation)
		citation := citationPrecision(observation, relevant)
		abstention := abstentionQuality(observation)
		recallSum += recall
		mrrSum += reciprocal
		ndcgSum += gain
		selectivitySum += selectivity
		citationSum += citation
		abstentionSum += abstention
		if observation.IngestionLag > metrics.IngestionLag {
			metrics.IngestionLag = observation.IngestionLag
		}
		if observation.TombstoneLag > metrics.TombstoneLag {
			metrics.TombstoneLag = observation.TombstoneLag
		}

		scores := []domain.EvaluationScore{
			{Metric: domain.MetricRecallAtK, Value: recall, Confidence: 1, Evaluator: version},
			{Metric: domain.MetricMRR, Value: reciprocal, Confidence: 1, Evaluator: version},
			{Metric: domain.MetricNDCG, Value: gain, Confidence: 1, Evaluator: version},
			{Metric: domain.MetricFilterSelectivity, Value: selectivity, Confidence: 1, Evaluator: version},
			{Metric: domain.MetricCitationPrecision, Value: citation, Confidence: 1, Evaluator: version},
			{Metric: domain.MetricAbstentionQuality, Value: abstention, Confidence: 1, Evaluator: version},
			{Metric: domain.MetricAuthorizationLeaks, Value: float64(leaks), Confidence: 1, Evaluator: version, Critical: leaks > 0,
				Rationale: fmt.Sprintf("%d matches outside principal %q entitlements", leaks, observation.Query.Principal)},
		}
		if scorer.MaxIngestionLag > 0 {
			scores = append(scores, scoreOf(domain.MetricIngestionLagMs, observation.IngestionLag <= scorer.MaxIngestionLag, version, false,
				fmt.Sprintf("ingestion lag %s", observation.IngestionLag)))
		}
		if scorer.MaxTombstoneLag > 0 {
			scores = append(scores, scoreOf(domain.MetricTombstoneLagMs, observation.TombstoneLag <= scorer.MaxTombstoneLag, version, observation.TombstoneLag > scorer.MaxTombstoneLag,
				fmt.Sprintf("tombstone lag %s", observation.TombstoneLag)))
		}
		results = append(results, domain.EvaluationResult{
			ManifestID: manifestID, ExampleID: observation.Query.QueryID, Role: domain.RoleCandidate, Scores: scores,
			NeedsHumanReview: leaks > 0, Reason: leakReason(leaks),
		})
	}
	samples := float64(len(observations))
	metrics.RecallAtK = recallSum / samples
	metrics.MRR = mrrSum / samples
	metrics.NDCG = ndcgSum / samples
	metrics.FilterSelectivity = selectivitySum / samples
	metrics.CitationPrecision = citationSum / samples
	metrics.AbstentionQuality = abstentionSum / samples
	return metrics, results, nil
}

func leakReason(leaks int) string {
	if leaks == 0 {
		return ""
	}
	return fmt.Sprintf("%d authorization leaks", leaks)
}

// rankedDocuments keeps the first K distinct documents in rank order.
func rankedDocuments(matches []domain.KnowledgeMatch, k int) []string {
	seen := make(map[string]struct{}, len(matches))
	ranked := make([]string, 0, k)
	for _, match := range matches {
		if _, dup := seen[match.DocumentID]; dup {
			continue
		}
		seen[match.DocumentID] = struct{}{}
		ranked = append(ranked, match.DocumentID)
		if len(ranked) == k {
			break
		}
	}
	return ranked
}

func recallAtK(ranked []string, relevant map[string]struct{}) float64 {
	if len(relevant) == 0 {
		return 1
	}
	found := 0
	for _, id := range ranked {
		if _, ok := relevant[id]; ok {
			found++
		}
	}
	return float64(found) / float64(len(relevant))
}

func reciprocalRank(ranked []string, relevant map[string]struct{}) float64 {
	for index, id := range ranked {
		if _, ok := relevant[id]; ok {
			return 1 / float64(index+1)
		}
	}
	return 0
}

// ndcgAtK uses binary relevance with log2 discounting against the ideal ranking.
func ndcgAtK(ranked []string, relevant map[string]struct{}) float64 {
	if len(relevant) == 0 {
		return 1
	}
	var gain float64
	for index, id := range ranked {
		if _, ok := relevant[id]; ok {
			gain += 1 / math.Log2(float64(index+2))
		}
	}
	ideal := 0.0
	for index := range min(len(relevant), len(ranked)) {
		ideal += 1 / math.Log2(float64(index+2))
	}
	if ideal == 0 {
		return 0
	}
	return gain / ideal
}

// filterSelectivity is the share of the pre-filter candidate population that survived authorization filtering.
func filterSelectivity(observation domain.RetrievalObservation) float64 {
	if observation.CandidateCount <= 0 {
		return 0
	}
	return clamp01(float64(len(observation.Matches)) / float64(observation.CandidateCount))
}

// citationPrecision is the share of cited documents that are both retrieved and expected.
func citationPrecision(observation domain.RetrievalObservation, relevant map[string]struct{}) float64 {
	if len(observation.CitedDocumentIDs) == 0 {
		return 1
	}
	retrieved := make(map[string]struct{}, len(observation.Matches))
	for index, match := range observation.Matches {
		if observation.Authorized[index] {
			retrieved[match.DocumentID] = struct{}{}
		}
	}
	supported := 0
	for _, id := range observation.CitedDocumentIDs {
		if _, seen := retrieved[id]; !seen {
			continue
		}
		if _, expected := relevant[id]; expected || len(relevant) == 0 {
			supported++
		}
	}
	return float64(supported) / float64(len(observation.CitedDocumentIDs))
}

// abstentionQuality scores whether the run abstained exactly when the golden query expected it.
func abstentionQuality(observation domain.RetrievalObservation) float64 {
	if observation.Abstained == observation.Query.ExpectAbstention {
		return 1
	}
	return 0
}
