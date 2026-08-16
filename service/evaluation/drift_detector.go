package evaluation

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ScoreDriftDetector compares a current score window against a reference by
// pooled effect size and reports drift only once it persists across windows.
type ScoreDriftDetector struct {
	// MinSamples per window before an effect is trusted.
	MinSamples int
	// MinEffectSize is the practical Cohen's d threshold, e.g. 0.3.
	MinEffectSize float64
	// SustainWindows is how many consecutive detections make drift sustained; zero means 2.
	SustainWindows int
	// MaxTrackedMetrics bounds per-metric streaks; zero means 1024.
	MaxTrackedMetrics int

	mu      sync.Mutex
	streaks map[string]int
}

var _ contract.DriftDetector = (*ScoreDriftDetector)(nil)

// Detect returns the effect size for the metric and whether drift is sustained.
func (detector *ScoreDriftDetector) Detect(_ context.Context, metric string, reference, current []float64) (domain.DriftReport, error) {
	if detector.MinSamples <= 1 || detector.MinEffectSize <= 0 || detector.SustainWindows < 0 || detector.MaxTrackedMetrics < 0 {
		return domain.DriftReport{}, fmt.Errorf("%w: drift detector needs min samples above one, a positive effect size, and non-negative windows", domain.ErrValidation)
	}
	if strings.TrimSpace(metric) == "" {
		return domain.DriftReport{}, fmt.Errorf("%w: metric is required", domain.ErrValidation)
	}
	report := domain.DriftReport{Metric: metric, ReferenceMean: mean(reference), CurrentMean: mean(current), Samples: len(current)}
	if len(reference) < detector.MinSamples || len(current) < detector.MinSamples {
		detector.record(metric, false)
		return report, nil
	}
	pooled := math.Sqrt((variance(reference)*float64(len(reference)-1) + variance(current)*float64(len(current)-1)) / float64(len(reference)+len(current)-2))
	switch {
	case pooled == 0 && report.CurrentMean == report.ReferenceMean:
		report.EffectSize = 0
	case pooled == 0:
		report.EffectSize = math.Copysign(math.Inf(1), report.CurrentMean-report.ReferenceMean)
	default:
		report.EffectSize = (report.CurrentMean - report.ReferenceMean) / pooled
	}
	detected := math.Abs(report.EffectSize) >= detector.MinEffectSize
	report.Sustained = detector.record(metric, detected)
	return report, nil
}

func (detector *ScoreDriftDetector) record(metric string, detected bool) bool {
	detector.mu.Lock()
	defer detector.mu.Unlock()
	if detector.streaks == nil {
		detector.streaks = make(map[string]int)
	}
	if !detected {
		delete(detector.streaks, metric)
		return false
	}
	limit := detector.MaxTrackedMetrics
	if limit == 0 {
		limit = 1024
	}
	if _, tracked := detector.streaks[metric]; !tracked && len(detector.streaks) >= limit {
		// Bounded: an untracked metric past capacity is reported per call only.
		return detector.SustainWindows <= 1
	}
	detector.streaks[metric]++
	windows := detector.SustainWindows
	if windows == 0 {
		windows = 2
	}
	return detector.streaks[metric] >= windows
}

func variance(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	average := mean(values)
	var sum float64
	for _, value := range values {
		sum += (value - average) * (value - average)
	}
	return sum / float64(len(values)-1)
}
