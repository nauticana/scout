package evaluation

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Calibrator measures judge–human agreement: Cohen's κ on pass/fail at
// PassThreshold, Krippendorff's α on interval scores, precision/recall on
// critical failures, and position and self-preference bias from swapped trials.
type Calibrator struct {
	// PassThreshold splits scores into pass/fail for κ; zero means 0.5.
	PassThreshold float64
	// MinLabels below which the report is refused as unreliable.
	MinLabels int
}

var _ contract.JudgeCalibrator = (*Calibrator)(nil)

func (calibrator *Calibrator) threshold() float64 {
	if calibrator.PassThreshold > 0 {
		return calibrator.PassThreshold
	}
	return 0.5
}

// Calibrate returns the agreement report for one judge version.
func (calibrator *Calibrator) Calibrate(_ context.Context, judge domain.EvaluatorVersion, labels []domain.CalibrationLabel, trials []domain.PositionTrial) (domain.CalibrationReport, error) {
	if calibrator.MinLabels <= 0 || calibrator.PassThreshold < 0 || calibrator.PassThreshold > 1 {
		return domain.CalibrationReport{}, fmt.Errorf("%w: calibrator needs positive min labels and a threshold in [0,1]", domain.ErrValidation)
	}
	if strings.TrimSpace(judge.Version) == "" {
		return domain.CalibrationReport{}, fmt.Errorf("%w: judge version is required", domain.ErrValidation)
	}
	if len(labels) < calibrator.MinLabels {
		return domain.CalibrationReport{}, fmt.Errorf("%w: %d calibration labels below minimum %d", domain.ErrValidation, len(labels), calibrator.MinLabels)
	}
	report := domain.CalibrationReport{JudgeVersion: judge, Samples: len(labels)}
	report.Kappa = cohensKappa(labels, calibrator.threshold())
	report.Alpha = krippendorffAlpha(labels)
	report.Precision, report.Recall = criticalPrecisionRecall(labels)
	report.PositionBias, report.SelfPreferenceBias = biases(trials)
	return report, nil
}

func cohensKappa(labels []domain.CalibrationLabel, threshold float64) float64 {
	var both, humanOnly, judgeOnly, neither float64
	for _, label := range labels {
		human, judge := label.HumanValue >= threshold, label.JudgeValue >= threshold
		switch {
		case human && judge:
			both++
		case human:
			humanOnly++
		case judge:
			judgeOnly++
		default:
			neither++
		}
	}
	n := float64(len(labels))
	observed := (both + neither) / n
	expected := ((both+humanOnly)/n)*((both+judgeOnly)/n) + ((judgeOnly+neither)/n)*((humanOnly+neither)/n)
	if expected == 1 {
		return 1
	}
	return (observed - expected) / (1 - expected)
}

// krippendorffAlpha is the interval-metric α for two coders over every labeled unit.
func krippendorffAlpha(labels []domain.CalibrationLabel) float64 {
	n := float64(len(labels))
	var observed float64
	pooled := make([]float64, 0, 2*len(labels))
	for _, label := range labels {
		diff := label.HumanValue - label.JudgeValue
		observed += diff * diff
		pooled = append(pooled, label.HumanValue, label.JudgeValue)
	}
	observed /= n
	total := float64(len(pooled))
	average := mean(pooled)
	var spread float64
	for _, value := range pooled {
		spread += (value - average) * (value - average)
	}
	expected := 2 * spread / (total - 1)
	if expected == 0 {
		if observed == 0 {
			return 1
		}
		return 0
	}
	return 1 - observed/expected
}

func criticalPrecisionRecall(labels []domain.CalibrationLabel) (float64, float64) {
	var truePositive, judgeCritical, humanCritical float64
	for _, label := range labels {
		if label.JudgeCritical {
			judgeCritical++
		}
		if label.HumanCritical {
			humanCritical++
		}
		if label.JudgeCritical && label.HumanCritical {
			truePositive++
		}
	}
	precision, recall := 1.0, 1.0
	if judgeCritical > 0 {
		precision = truePositive / judgeCritical
	}
	if humanCritical > 0 {
		recall = truePositive / humanCritical
	}
	return precision, recall
}

// biases returns the flip rate across presentation orders and the excess rate
// of preferring the judge's own model family over one half.
func biases(trials []domain.PositionTrial) (float64, float64) {
	if len(trials) == 0 {
		return 0, 0
	}
	var flips, familyTrials, familyPreferred float64
	for _, trial := range trials {
		if trial.PreferredAFirst != trial.PreferredASwapped {
			flips++
		}
		if trial.AIsJudgeFamily {
			familyTrials += 2
			if trial.PreferredAFirst {
				familyPreferred++
			}
			if trial.PreferredASwapped {
				familyPreferred++
			}
		}
	}
	selfPreference := 0.0
	if familyTrials > 0 {
		selfPreference = familyPreferred/familyTrials - 0.5
	}
	return flips / float64(len(trials)), selfPreference
}
