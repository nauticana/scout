package evaluation

import (
	"math"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func result(manifestID, exampleID string, role domain.EvaluationRole, value float64, critical bool) domain.EvaluationResult {
	return domain.EvaluationResult{
		ManifestID: manifestID, ExampleID: exampleID, Role: role, Latency: 100 * time.Millisecond,
		Scores: []domain.EvaluationScore{{Metric: domain.MetricCorrectness, Value: value, Confidence: 1, Critical: critical, Evaluator: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"}}},
	}
}

func TestPairedScorerComputesDeltasAndBootstrapCIOnFixedSeed(t *testing.T) {
	examples := []domain.GoldenExample{testExample("a"), testExample("b"), testExample("c"), testExample("d")}
	manifest := testManifest(t, examples...)
	var results []domain.EvaluationResult
	for _, example := range examples {
		results = append(results, result(manifest.ManifestID, example.ExampleID, domain.RoleBaseline, 0.5, false))
		results = append(results, result(manifest.ManifestID, example.ExampleID, domain.RoleCandidate, 1, false))
	}
	scorer := &PairedScorer{Seed: 11, Resamples: 200, Policy: domain.PromotionPolicy{MinSamples: 4, MaxCriticalFailures: 0, Tolerances: map[string]float64{domain.MetricCorrectness: 0.05}}}

	summary, err := scorer.Score(manifest.ManifestID, examples, results)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Samples != 4 {
		t.Fatalf("samples = %d", summary.Samples)
	}
	var aggregate domain.SliceDelta
	for _, delta := range summary.Deltas {
		if delta.Slice == domain.SliceAll && delta.Metric == domain.MetricCorrectness {
			aggregate = delta
		}
	}
	if math.Abs(aggregate.Delta-0.5) > 1e-9 || aggregate.Baseline != 0.5 || aggregate.Candidate != 1 || aggregate.Samples != 4 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
	// Every paired difference is identical, so the bootstrap interval collapses onto the delta.
	if math.Abs(aggregate.CILow-0.5) > 1e-9 || math.Abs(aggregate.CIHigh-0.5) > 1e-9 {
		t.Fatalf("degenerate CI = [%f, %f]", aggregate.CILow, aggregate.CIHigh)
	}
	if !summary.Promotable {
		t.Fatalf("uniform improvement is not promotable: %v", summary.Reasons)
	}
	// The same seed must reproduce the interval exactly.
	repeat, err := scorer.Score(manifest.ManifestID, examples, results)
	if err != nil || repeat.Deltas[0] != summary.Deltas[0] {
		t.Fatalf("seeded bootstrap is not reproducible: %+v vs %+v (%v)", repeat.Deltas[0], summary.Deltas[0], err)
	}
	// Slices are reported alongside the aggregate.
	found := map[string]bool{}
	for _, delta := range summary.Deltas {
		found[delta.Slice] = true
	}
	for _, slice := range []string{domain.SliceAll, "domain:billing", "language:en-US", "risk:low"} {
		if !found[slice] {
			t.Fatalf("missing slice %q in %+v", slice, summary.Deltas)
		}
	}
}

func TestPairedScorerBootstrapIntervalBracketsTheMeanOnMixedData(t *testing.T) {
	values := []float64{0.9, 0.1, 0.8, 0.2, 0.7, 0.3, 0.6, 0.4}
	examples := make([]domain.GoldenExample, len(values))
	var results []domain.EvaluationResult
	manifest := testManifest(t)
	for i, value := range values {
		examples[i] = testExample(string(rune('a' + i)))
		results = append(results,
			result(manifest.ManifestID, examples[i].ExampleID, domain.RoleBaseline, 0.5, false),
			result(manifest.ManifestID, examples[i].ExampleID, domain.RoleCandidate, value, false))
	}
	scorer := &PairedScorer{Seed: 5, Resamples: 500, Policy: domain.PromotionPolicy{MinSamples: 4, Tolerances: map[string]float64{domain.MetricCorrectness: 0.5}, ConfidenceLevel: 0.9}}
	summary, err := scorer.Score(manifest.ManifestID, examples, results)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range summary.Deltas {
		if delta.Slice != domain.SliceAll || delta.Metric != domain.MetricCorrectness {
			continue
		}
		if math.Abs(delta.Delta) > 1e-9 {
			t.Fatalf("symmetric data should have a zero mean delta: %+v", delta)
		}
		if !(delta.CILow < delta.Delta && delta.Delta < delta.CIHigh) {
			t.Fatalf("CI does not bracket the delta: %+v", delta)
		}
	}
}

func TestPairedScorerPromotionRules(t *testing.T) {
	examples := []domain.GoldenExample{testExample("a"), testExample("b")}
	examples[1].RiskTier = "high"
	manifest := testManifest(t, examples...)
	policy := domain.PromotionPolicy{MinSamples: 2, MaxCriticalFailures: 0, HighRiskTiers: []string{"high"}, Tolerances: map[string]float64{domain.MetricCorrectness: 0.01}}
	scorer := &PairedScorer{Seed: 3, Resamples: 100, Policy: policy}

	t.Run("critical failure blocks", func(t *testing.T) {
		results := []domain.EvaluationResult{
			result(manifest.ManifestID, "a", domain.RoleBaseline, 1, false), result(manifest.ManifestID, "a", domain.RoleCandidate, 1, true),
			result(manifest.ManifestID, "b", domain.RoleBaseline, 1, false), result(manifest.ManifestID, "b", domain.RoleCandidate, 1, false),
		}
		summary, err := scorer.Score(manifest.ManifestID, examples, results)
		if err != nil || summary.Promotable || summary.CriticalFailures != 1 {
			t.Fatalf("summary = %+v, %v", summary, err)
		}
	})

	t.Run("protected slice regression blocks", func(t *testing.T) {
		results := []domain.EvaluationResult{
			result(manifest.ManifestID, "a", domain.RoleBaseline, 1, false), result(manifest.ManifestID, "a", domain.RoleCandidate, 1, false),
			result(manifest.ManifestID, "b", domain.RoleBaseline, 1, false), result(manifest.ManifestID, "b", domain.RoleCandidate, 0, false),
		}
		summary, err := scorer.Score(manifest.ManifestID, examples, results)
		if err != nil || summary.Promotable {
			t.Fatalf("summary = %+v, %v", summary, err)
		}
	})

	t.Run("too few samples blocks", func(t *testing.T) {
		results := []domain.EvaluationResult{
			result(manifest.ManifestID, "a", domain.RoleBaseline, 1, false), result(manifest.ManifestID, "a", domain.RoleCandidate, 1, false),
		}
		summary, err := scorer.Score(manifest.ManifestID, examples, results)
		if err != nil || summary.Promotable || summary.Samples != 1 {
			t.Fatalf("summary = %+v, %v", summary, err)
		}
	})
}

func TestPairedScorerDecidedStopsOnlyWhenTheEffectIsSettled(t *testing.T) {
	scorer := &PairedScorer{Seed: 1, Policy: domain.PromotionPolicy{MinSamples: 2, Tolerances: map[string]float64{domain.MetricCorrectness: 0.02}}}
	undecided := domain.EvaluationSummary{Deltas: []domain.SliceDelta{{Slice: domain.SliceAll, Metric: domain.MetricCorrectness, CILow: -0.1, CIHigh: 0.1, Samples: 10}}}
	improved := domain.EvaluationSummary{Deltas: []domain.SliceDelta{{Slice: domain.SliceAll, Metric: domain.MetricCorrectness, CILow: 0.05, CIHigh: 0.2, Samples: 10}}}
	regressed := domain.EvaluationSummary{Deltas: []domain.SliceDelta{{Slice: domain.SliceAll, Metric: domain.MetricCorrectness, CILow: -0.3, CIHigh: -0.2, Samples: 10}}}
	tooFew := domain.EvaluationSummary{Deltas: []domain.SliceDelta{{Slice: domain.SliceAll, Metric: domain.MetricCorrectness, CILow: 0.05, CIHigh: 0.2, Samples: 1}}}
	if scorer.Decided(undecided) || !scorer.Decided(improved) || !scorer.Decided(regressed) || scorer.Decided(tooFew) {
		t.Fatal("early-stop decision is wrong")
	}
}

func TestAblationMatrixChangesOneComponentPerArm(t *testing.T) {
	baseline := domain.EvaluationSubject{AgentID: "a", AgentVersion: "v1", Versions: domain.ComponentVersions{Model: "m1", Prompt: "p1", Index: "i1"}, Decoding: []byte(`{"t":0}`)}
	candidate := domain.EvaluationSubject{AgentID: "a", AgentVersion: "v2", Versions: domain.ComponentVersions{Model: "m2", Prompt: "p2", Index: "i1"}, IndexGeneration: 4, Decoding: []byte(`{"t":1}`)}
	arms := AblationMatrix(baseline, candidate)
	if len(arms) != 4 {
		t.Fatalf("arms = %+v", arms)
	}
	for _, arm := range arms {
		changes := 0
		if arm.Subject.Versions.Model != baseline.Versions.Model {
			changes++
		}
		if arm.Subject.Versions.Prompt != baseline.Versions.Prompt {
			changes++
		}
		if arm.Subject.IndexGeneration != baseline.IndexGeneration {
			changes++
		}
		if string(arm.Subject.Decoding) != string(baseline.Decoding) {
			changes++
		}
		if changes != 1 {
			t.Fatalf("arm %q changed %d components", arm.Component, changes)
		}
	}
}

func TestPairedScorerRejectsForeignResultsAndBadConfig(t *testing.T) {
	examples := []domain.GoldenExample{testExample("a")}
	manifest := testManifest(t, examples...)
	scorer := &PairedScorer{Policy: domain.PromotionPolicy{MinSamples: 1}}
	if _, err := scorer.Score(manifest.ManifestID, examples, []domain.EvaluationResult{result("other", "a", domain.RoleBaseline, 1, false)}); err == nil {
		t.Fatal("accepted a result from another manifest")
	}
	if _, err := scorer.Score(manifest.ManifestID, examples, []domain.EvaluationResult{result(manifest.ManifestID, "zz", domain.RoleBaseline, 1, false)}); err == nil {
		t.Fatal("accepted a result for an unknown example")
	}
	if _, err := (&PairedScorer{}).Score(manifest.ManifestID, examples, nil); err == nil {
		t.Fatal("accepted a zero promotion policy")
	}
}
