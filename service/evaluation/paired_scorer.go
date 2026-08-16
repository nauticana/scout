package evaluation

import (
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"

	"github.com/nauticana/scout/domain"
)

// PairedScorer turns per-arm results into per-slice paired deltas with
// bootstrap confidence intervals and applies the promotion policy.
type PairedScorer struct {
	// Seed makes the bootstrap reproducible.
	Seed int64
	// Resamples is the bootstrap iteration count; zero means 1000.
	Resamples int
	Policy    domain.PromotionPolicy
}

const (
	slicePrefixDomain   = "domain:"
	slicePrefixLanguage = "language:"
	slicePrefixRisk     = "risk:"
)

func (scorer *PairedScorer) validate() error {
	if scorer.Resamples < 0 || scorer.Policy.MinSamples <= 0 || scorer.Policy.MaxCriticalFailures < 0 {
		return fmt.Errorf("%w: paired scorer needs positive min samples and non-negative resamples and critical failures", domain.ErrValidation)
	}
	if level := scorer.Policy.ConfidenceLevel; level != 0 && (level <= 0 || level >= 1) {
		return fmt.Errorf("%w: confidence level must be in (0,1)", domain.ErrValidation)
	}
	for metric, tolerance := range scorer.Policy.Tolerances {
		if strings.TrimSpace(metric) == "" || tolerance < 0 || math.IsNaN(tolerance) {
			return fmt.Errorf("%w: tolerance for %q must be non-negative", domain.ErrValidation, metric)
		}
	}
	return nil
}

func (scorer *PairedScorer) primaryMetric() string {
	return metricOr(scorer.Policy.PrimaryMetric, domain.MetricCorrectness)
}

type pairedValues struct {
	baseline, candidate []float64
}

// Score builds the summary for one manifest from both arms' results.
func (scorer *PairedScorer) Score(manifestID string, examples []domain.GoldenExample, results []domain.EvaluationResult) (domain.EvaluationSummary, error) {
	if err := scorer.validate(); err != nil {
		return domain.EvaluationSummary{}, err
	}
	byExample := make(map[string]domain.GoldenExample, len(examples))
	for _, example := range examples {
		byExample[example.ExampleID] = example
	}
	type key struct{ slice, metric string }
	arms := make(map[string]map[domain.EvaluationRole]map[string]float64)
	summary := domain.EvaluationSummary{ManifestID: manifestID}
	var totalUsage domain.Usage
	for _, result := range results {
		if result.ManifestID != manifestID {
			return domain.EvaluationSummary{}, fmt.Errorf("%w: result for %q does not belong to manifest %q", domain.ErrValidation, result.ExampleID, manifestID)
		}
		if _, known := byExample[result.ExampleID]; !known {
			return domain.EvaluationSummary{}, fmt.Errorf("%w: result for unknown example %q", domain.ErrValidation, result.ExampleID)
		}
		if result.NeedsHumanReview {
			summary.HumanReviewPending++
		}
		if result.Role == domain.RoleCandidate {
			for _, score := range result.Scores {
				if score.Critical {
					summary.CriticalFailures++
					break
				}
			}
		}
		totalUsage = addUsage(totalUsage, result.Usage)
		if arms[result.ExampleID] == nil {
			arms[result.ExampleID] = make(map[domain.EvaluationRole]map[string]float64)
		}
		arms[result.ExampleID][result.Role] = metricMeans(result)
	}
	summary.Usage = totalUsage

	pairs := make(map[key]*pairedValues)
	exampleIDs := make([]string, 0, len(arms))
	for id := range arms {
		exampleIDs = append(exampleIDs, id)
	}
	sort.Strings(exampleIDs)
	for _, id := range exampleIDs {
		baseline, hasBaseline := arms[id][domain.RoleBaseline]
		candidate, hasCandidate := arms[id][domain.RoleCandidate]
		if !hasBaseline || !hasCandidate {
			continue
		}
		summary.Samples++
		for _, slice := range slicesOf(byExample[id]) {
			for metric, baseValue := range baseline {
				candValue, ok := candidate[metric]
				if !ok {
					continue
				}
				k := key{slice, metric}
				if pairs[k] == nil {
					pairs[k] = &pairedValues{}
				}
				pairs[k].baseline = append(pairs[k].baseline, baseValue)
				pairs[k].candidate = append(pairs[k].candidate, candValue)
			}
		}
	}
	keys := make([]key, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].slice != keys[j].slice {
			return keys[i].slice < keys[j].slice
		}
		return keys[i].metric < keys[j].metric
	})
	for _, k := range keys {
		summary.Deltas = append(summary.Deltas, scorer.delta(k.slice, k.metric, pairs[k]))
	}
	summary.Promotable, summary.Reasons = scorer.promotable(summary)
	return summary, nil
}

// Decided reports whether the primary metric's aggregate effect is settled either way, allowing early stopping.
func (scorer *PairedScorer) Decided(summary domain.EvaluationSummary) bool {
	primary := scorer.primaryMetric()
	for _, delta := range summary.Deltas {
		if delta.Slice != domain.SliceAll || delta.Metric != primary || delta.Samples < scorer.Policy.MinSamples {
			continue
		}
		tolerance := scorer.Policy.Tolerances[primary]
		return delta.CILow > 0 || delta.CIHigh < -tolerance
	}
	return false
}

func (scorer *PairedScorer) delta(slice, metric string, values *pairedValues) domain.SliceDelta {
	n := len(values.baseline)
	diffs := make([]float64, n)
	var sumBase, sumCand float64
	for i := range values.baseline {
		diffs[i] = values.candidate[i] - values.baseline[i]
		sumBase += values.baseline[i]
		sumCand += values.candidate[i]
	}
	delta := domain.SliceDelta{Slice: slice, Metric: metric, Baseline: sumBase / float64(n), Candidate: sumCand / float64(n), Delta: mean(diffs), Samples: n}
	delta.CILow, delta.CIHigh = scorer.bootstrapCI(diffs, sliceSeed(scorer.Seed, slice, metric))
	return delta
}

// bootstrapCI returns the percentile interval of the resampled mean difference.
func (scorer *PairedScorer) bootstrapCI(diffs []float64, seed uint64) (float64, float64) {
	if len(diffs) < 2 {
		value := mean(diffs)
		return value, value
	}
	resamples := scorer.Resamples
	if resamples == 0 {
		resamples = 1000
	}
	level := scorer.Policy.ConfidenceLevel
	if level == 0 {
		level = 0.95
	}
	generator := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	means := make([]float64, resamples)
	for r := range means {
		var sum float64
		for range diffs {
			sum += diffs[generator.IntN(len(diffs))]
		}
		means[r] = sum / float64(len(diffs))
	}
	sort.Float64s(means)
	lower := int(math.Floor((1 - level) / 2 * float64(resamples-1)))
	upper := int(math.Ceil((1 - (1-level)/2) * float64(resamples-1)))
	return means[lower], means[upper]
}

func (scorer *PairedScorer) promotable(summary domain.EvaluationSummary) (bool, []string) {
	var reasons []string
	if summary.Samples < scorer.Policy.MinSamples {
		reasons = append(reasons, fmt.Sprintf("samples %d below minimum %d", summary.Samples, scorer.Policy.MinSamples))
	}
	if summary.CriticalFailures > scorer.Policy.MaxCriticalFailures {
		reasons = append(reasons, fmt.Sprintf("%d critical failures exceed %d", summary.CriticalFailures, scorer.Policy.MaxCriticalFailures))
	}
	protected := map[string]struct{}{domain.SliceAll: {}}
	for _, slice := range scorer.Policy.ProtectedSlices {
		protected[slice] = struct{}{}
	}
	for _, tier := range scorer.Policy.HighRiskTiers {
		protected[slicePrefixRisk+tier] = struct{}{}
	}
	primary := scorer.primaryMetric()
	seenPrimary := false
	for _, delta := range summary.Deltas {
		if _, guarded := protected[delta.Slice]; !guarded {
			continue
		}
		tolerance, guardedMetric := scorer.Policy.Tolerances[delta.Metric]
		if delta.Slice == domain.SliceAll && delta.Metric == primary {
			seenPrimary = true
			if !guardedMetric {
				tolerance, guardedMetric = 0, true
			}
		}
		if !guardedMetric {
			continue
		}
		if delta.CILow < -tolerance {
			reasons = append(reasons, fmt.Sprintf("%s on %s may regress by %.4f (tolerance %.4f)", delta.Metric, delta.Slice, -delta.CILow, tolerance))
		}
	}
	if !seenPrimary {
		reasons = append(reasons, fmt.Sprintf("no paired samples for primary metric %s", primary))
	}
	return len(reasons) == 0, reasons
}

// AblationMatrix returns one arm per component that differs between baseline
// and candidate, each changing only that component, for regression attribution.
func AblationMatrix(baseline, candidate domain.EvaluationSubject) []domain.AblationArm {
	var arms []domain.AblationArm
	add := func(component string, mutate func(*domain.EvaluationSubject)) {
		arm := baseline
		mutate(&arm)
		arms = append(arms, domain.AblationArm{Component: component, Subject: arm})
	}
	b, c := baseline.Versions, candidate.Versions
	if b.Model != c.Model {
		add("model", func(s *domain.EvaluationSubject) { s.Versions.Model = c.Model })
	}
	if b.Prompt != c.Prompt {
		add("prompt", func(s *domain.EvaluationSubject) { s.Versions.Prompt = c.Prompt })
	}
	if b.Knowledge != c.Knowledge {
		add("knowledge", func(s *domain.EvaluationSubject) { s.Versions.Knowledge = c.Knowledge })
	}
	if b.Index != c.Index || baseline.IndexGeneration != candidate.IndexGeneration {
		add("index", func(s *domain.EvaluationSubject) {
			s.Versions.Index, s.IndexGeneration = c.Index, candidate.IndexGeneration
		})
	}
	if b.Tool != c.Tool {
		add("tool", func(s *domain.EvaluationSubject) { s.Versions.Tool = c.Tool })
	}
	if b.Guardrail != c.Guardrail {
		add("guardrail", func(s *domain.EvaluationSubject) { s.Versions.Guardrail = c.Guardrail })
	}
	if !bytes.Equal(baseline.Decoding, candidate.Decoding) {
		add("decoding", func(s *domain.EvaluationSubject) { s.Decoding = append([]byte(nil), candidate.Decoding...) })
	}
	return arms
}

func slicesOf(example domain.GoldenExample) []string {
	slices := []string{domain.SliceAll}
	if example.Domain != "" {
		slices = append(slices, slicePrefixDomain+example.Domain)
	}
	if example.Language != "" {
		slices = append(slices, slicePrefixLanguage+example.Language)
	}
	if example.RiskTier != "" {
		slices = append(slices, slicePrefixRisk+example.RiskTier)
	}
	return slices
}

// metricMeans averages every evaluator's value per metric and adds the implicit latency, token, and cost metrics.
func metricMeans(result domain.EvaluationResult) map[string]float64 {
	sums := make(map[string]float64)
	counts := make(map[string]int)
	for _, score := range result.Scores {
		sums[score.Metric] += score.Value
		counts[score.Metric]++
	}
	means := make(map[string]float64, len(sums)+3)
	for metric, sum := range sums {
		means[metric] = sum / float64(counts[metric])
	}
	means[domain.MetricLatencyMs] = float64(result.Latency.Milliseconds())
	means[domain.MetricTokens] = float64(result.Usage.InputTokens + result.Usage.OutputTokens)
	means[domain.MetricCostMinorUnits] = float64(result.Usage.CostMinorUnits)
	return means
}

func addUsage(total, usage domain.Usage) domain.Usage {
	total.InputTokens += usage.InputTokens
	total.OutputTokens += usage.OutputTokens
	total.ToolCalls += usage.ToolCalls
	total.CostMinorUnits += usage.CostMinorUnits
	if total.Currency == "" {
		total.Currency = usage.Currency
	}
	return total
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func sliceSeed(seed int64, slice, metric string) uint64 {
	return uint64(seed) ^ fnvSum(slice+"|"+metric)
}
