package evaluation

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestAdjudicatorDeduplicatesByDigestAndRequiresAVerdict(t *testing.T) {
	ctx := context.Background()
	adjudicator := &Adjudicator{Now: fixedClock(testClock)}
	failure := []byte("tool timeout on refund lookup")

	first, err := adjudicator.Report(ctx, 7, "s1", failure)
	if err != nil {
		t.Fatal(err)
	}
	second, err := adjudicator.Report(ctx, 7, "s2", failure)
	if err != nil || second.FailureID != first.FailureID || second.Occurrences != 2 {
		t.Fatalf("dedupe = %+v, %v", second, err)
	}
	other, err := adjudicator.Report(ctx, 7, "s3", []byte("different failure"))
	if err != nil || other.FailureID == first.FailureID {
		t.Fatalf("distinct failure folded into the same bucket: %+v", other)
	}
	// A different tenant never shares a bucket.
	crossTenant, err := adjudicator.Report(ctx, 8, "s1", failure)
	if err != nil || crossTenant.Occurrences != 1 {
		t.Fatalf("cross-tenant = %+v, %v", crossTenant, err)
	}

	pending, err := adjudicator.Pending(ctx, 7, 10)
	if err != nil || len(pending) != 2 || pending[0].Occurrences != 2 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	accepted, err := adjudicator.Adjudicate(ctx, 7, first.FailureID, "quality-owner", true)
	if err != nil || !accepted.Adjudicated || !accepted.Accepted || accepted.Reviewer != "quality-owner" {
		t.Fatalf("adjudicated = %+v, %v", accepted, err)
	}
	if _, err := adjudicator.Adjudicate(ctx, 7, first.FailureID, "quality-owner", false); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("double adjudication = %v", err)
	}
	pending, _ = adjudicator.Pending(ctx, 7, 10)
	if len(pending) != 1 {
		t.Fatalf("adjudicated bucket still pending: %+v", pending)
	}
	if _, err := adjudicator.Adjudicate(ctx, 7, "unknown", "reviewer", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown failure = %v", err)
	}
}

func TestScoreDriftDetectorReportsSustainedEffectOnly(t *testing.T) {
	ctx := context.Background()
	detector := &ScoreDriftDetector{MinSamples: 4, MinEffectSize: 0.5, SustainWindows: 2}
	reference := []float64{0.80, 0.82, 0.78, 0.81, 0.79}
	drifted := []float64{0.50, 0.52, 0.48, 0.51, 0.49}

	first, err := detector.Detect(ctx, domain.MetricCorrectness, reference, drifted)
	if err != nil {
		t.Fatal(err)
	}
	if first.EffectSize > -1 || first.Sustained {
		t.Fatalf("first window = %+v", first)
	}
	second, err := detector.Detect(ctx, domain.MetricCorrectness, reference, drifted)
	if err != nil || !second.Sustained {
		t.Fatalf("second window = %+v, %v", second, err)
	}
	// A window back inside the band clears the streak.
	if _, err := detector.Detect(ctx, domain.MetricCorrectness, reference, reference); err != nil {
		t.Fatal(err)
	}
	recovered, _ := detector.Detect(ctx, domain.MetricCorrectness, reference, drifted)
	if recovered.Sustained {
		t.Fatalf("streak survived a clean window: %+v", recovered)
	}
	short, err := detector.Detect(ctx, domain.MetricCorrectness, reference, []float64{0.1})
	if err != nil || short.Sustained || short.EffectSize != 0 {
		t.Fatalf("short window = %+v, %v", short, err)
	}
	if _, err := (&ScoreDriftDetector{}).Detect(ctx, domain.MetricCorrectness, reference, drifted); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unconfigured detector = %v", err)
	}
}

func TestCalibratorComputesAgreementAndBias(t *testing.T) {
	ctx := context.Background()
	calibrator := &Calibrator{PassThreshold: 0.5, MinLabels: 4}
	agreeing := []domain.CalibrationLabel{
		{ExampleID: "a", Metric: domain.MetricCorrectness, HumanValue: 1, JudgeValue: 1, HumanCritical: true, JudgeCritical: true},
		{ExampleID: "b", Metric: domain.MetricCorrectness, HumanValue: 0, JudgeValue: 0},
		{ExampleID: "c", Metric: domain.MetricCorrectness, HumanValue: 1, JudgeValue: 1},
		{ExampleID: "d", Metric: domain.MetricCorrectness, HumanValue: 0, JudgeValue: 0},
	}
	report, err := calibrator.Calibrate(ctx, domain.EvaluatorVersion{Kind: "judge", Version: "j1"}, agreeing, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(report.Kappa-1) > 1e-9 || math.Abs(report.Alpha-1) > 1e-9 || report.Precision != 1 || report.Recall != 1 || report.Samples != 4 {
		t.Fatalf("perfect agreement = %+v", report)
	}

	disagreeing := []domain.CalibrationLabel{
		{HumanValue: 1, JudgeValue: 0, HumanCritical: true},
		{HumanValue: 0, JudgeValue: 1, JudgeCritical: true},
		{HumanValue: 1, JudgeValue: 0, HumanCritical: true},
		{HumanValue: 0, JudgeValue: 1, JudgeCritical: true},
	}
	report, err = calibrator.Calibrate(ctx, domain.EvaluatorVersion{Kind: "judge", Version: "j1"}, disagreeing, nil)
	if err != nil || report.Kappa >= 0 || report.Precision != 0 || report.Recall != 0 {
		t.Fatalf("full disagreement = %+v, %v", report, err)
	}

	trials := []domain.PositionTrial{
		{PreferredAFirst: true, PreferredASwapped: false, AIsJudgeFamily: true},
		{PreferredAFirst: true, PreferredASwapped: true, AIsJudgeFamily: true},
		{PreferredAFirst: false, PreferredASwapped: false},
		{PreferredAFirst: true, PreferredASwapped: true},
	}
	report, err = calibrator.Calibrate(ctx, domain.EvaluatorVersion{Kind: "judge", Version: "j1"}, agreeing, trials)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(report.PositionBias-0.25) > 1e-9 || math.Abs(report.SelfPreferenceBias-0.25) > 1e-9 {
		t.Fatalf("bias = %+v", report)
	}
	if _, err := calibrator.Calibrate(ctx, domain.EvaluatorVersion{Kind: "judge", Version: "j1"}, agreeing[:2], nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("too few labels = %v", err)
	}
}
