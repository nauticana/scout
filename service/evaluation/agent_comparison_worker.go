package evaluation

import (
	"context"
	"errors"
	"fmt"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/saturate"
	"github.com/nauticana/scout/internal/stage"
)

// compareExample answers one example with arm a (arms[1]) and arm b (arms[0]), stores both
// answers, has the pair judge decide between them unless an arm ended in a failed state,
// and records the outcome; it returns the cost it settled. Spend an arm or the judge
// incurred is recorded even when the example then fails, so a reclaimed attempt never pays
// for it again; such a record carries a reason and fails the run.
func (w *AgentEvaluationWorker) compareExample(ctx context.Context, claim domain.AgentEvaluationClaim, arms []domain.EvaluationArm, limits []domain.ExecutionLimits, example domain.GoldenExample) (int64, error) {
	a, err := w.execute(ctx, claim, arms[1], limits[1], example)
	if err != nil {
		return 0, &caseFailure{example: example, err: err}
	}
	a.Role = domain.RoleBaseline
	if err = w.settleable(a); err != nil {
		return 0, &caseFailure{example: example, err: err, unsettled: true}
	}
	comparison := domain.CaseComparison{
		ExampleID: example.ExampleID,
		A:         domain.EvaluationResult{ExampleID: example.ExampleID, Role: domain.RoleBaseline, Latency: a.Latency, Usage: a.Usage},
		B:         domain.EvaluationResult{ExampleID: example.ExampleID, Role: domain.RoleCandidate},
	}
	if err = ctx.Err(); err != nil {
		comparison.Reason = "arm b did not run: " + stage.ErrorClass(err)
		return w.settle(ctx, claim, example, comparison, err)
	}
	b, err := w.execute(ctx, claim, arms[0], limits[0], example)
	if err == nil {
		err = w.settleable(b)
	}
	if err != nil {
		comparison.Reason = "arm b failed: " + stage.ErrorClass(err)
		return w.settle(ctx, claim, example, comparison, &caseFailure{example: example, err: err, unsettled: errors.Is(err, domain.ErrValidation)})
	}
	b.Role = domain.RoleCandidate
	comparison.B.Latency, comparison.B.Usage = b.Latency, b.Usage
	if comparison.A.Output, comparison.B.Output, err = w.storedOutputs(ctx, claim.Run, arms, example, a, b); err != nil {
		comparison.Reason = "storing answers failed: " + stage.ErrorClass(err)
		return w.settle(ctx, claim, example, comparison, &caseFailure{example: example, err: err})
	}
	judgeErr := w.decide(ctx, claim.Run.RequestedBy, example, a, b, &comparison)
	if judgeErr != nil && ctx.Err() != nil {
		// Shutdown cut the judge off: nothing is recorded, so the reclaimed attempt compares the example again.
		return 0, ctx.Err()
	}
	var failure error
	if judgeErr != nil {
		failure = &caseFailure{example: example, err: judgeErr, unsettled: errors.Is(judgeErr, errJudgeSpendUnsettled)}
	}
	return w.settle(ctx, claim, example, comparison, failure)
}

// settle records the comparison under the lease, detached from cancellation, and returns the
// cost it settled with failure, the outcome the example ends in.
func (w *AgentEvaluationWorker) settle(ctx context.Context, claim domain.AgentEvaluationClaim, example domain.GoldenExample, comparison domain.CaseComparison, failure error) (int64, error) {
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claim.Lease.TTL)
	err := w.Queue.RecordComparison(recordCtx, claim.Lease, comparison)
	cancel()
	if errors.Is(err, domain.ErrValidation) {
		return 0, &caseFailure{example: example, err: err, unsettled: true}
	}
	if err != nil {
		return 0, fmt.Errorf("record comparison %q: %w", example.ExampleID, err)
	}
	cost := saturate.Add(saturate.Add(comparison.A.Usage.CostMinorUnits, comparison.B.Usage.CostMinorUnits), comparison.EvaluatorUsage.CostMinorUnits)
	return cost, failure
}

// storedOutputs keeps both answers: a baseline's recorded output, an executed answer written
// under its content digest, or nothing for an arm that ended in a failed state. The writes
// outlive cancellation so that answers paid for are kept.
func (w *AgentEvaluationWorker) storedOutputs(ctx context.Context, run domain.AgentEvaluationRun, arms []domain.EvaluationArm, example domain.GoldenExample, a, b domain.EvaluationCase) (domain.ObjectRef, domain.ObjectRef, error) {
	var refs [2]domain.ObjectRef
	for i, answer := range []struct {
		arm      domain.EvaluationArm
		evalCase domain.EvaluationCase
	}{{arms[1], a}, {arms[0], b}} {
		switch {
		case answer.arm.Subject.Kind == domain.SubjectBaseline:
			refs[i] = answer.arm.Outputs[example.ExampleID]
		case !stopped(answer.evalCase):
			ref, err := putContent(context.WithoutCancel(ctx), w.Outputs, w.OutputScheme, fmt.Sprintf("evaluation/output/%d/%d", run.TenantID, run.RunID), answer.evalCase.Output)
			if err != nil {
				return domain.ObjectRef{}, domain.ObjectRef{}, fmt.Errorf("store output of %q: %w", example.ExampleID, err)
			}
			refs[i] = ref
		}
	}
	return refs[0], refs[1], nil
}

// decide fills the comparison's verdict: an arm that ended in a failed state loses without a
// judge call, two such arms tie, and otherwise the pair judge decides as principal. A judging
// failure leaves no verdict, records why, and is returned with the judge's spend kept.
func (w *AgentEvaluationWorker) decide(ctx context.Context, principal domain.PrincipalRef, example domain.GoldenExample, a, b domain.EvaluationCase, comparison *domain.CaseComparison) error {
	stoppedA, stoppedB := stopped(a), stopped(b)
	switch {
	case stoppedA && stoppedB:
		comparison.Verdict, comparison.Confidence, comparison.Rationale = domain.ComparisonTie, 1, "both arms ended in a failed state"
		return nil
	case stoppedA:
		comparison.Verdict, comparison.Confidence, comparison.Rationale = domain.ComparisonPrefersB, 1, "arm a ended in a failed state"
		return nil
	case stoppedB:
		comparison.Verdict, comparison.Confidence, comparison.Rationale = domain.ComparisonPrefersA, 1, "arm b ended in a failed state"
		return nil
	}
	verdict, err := w.PairJudge.Compare(ctx, domain.EvaluationPair{Example: example, Baseline: a, Candidate: b, Principal: principal})
	var spendErr error
	comparison.EvaluatorUsage, spendErr = w.judgeSpend(verdict.Usage, verdict.Unpriced)
	if err = errors.Join(spendErr, err); err != nil {
		comparison.Reason = "judging failed: " + stage.ErrorClass(err)
		return fmt.Errorf("pair judge %s: %w", w.PairJudge.Version().Version, err)
	}
	comparison.Evaluator, comparison.InputDigest, comparison.Confidence = w.PairJudge.Version(), verdict.InputDigest, clamp01(verdict.Confidence)
	comparison.A.Scores, comparison.B.Scores = verdict.Baseline, verdict.Candidate
	if !example.Hidden {
		comparison.Rationale = verdict.Rationale
	}
	switch verdict.Preferred {
	case domain.RoleBaseline:
		comparison.Verdict = domain.ComparisonPrefersA
	case domain.RoleCandidate:
		comparison.Verdict = domain.ComparisonPrefersB
	default:
		comparison.Verdict = domain.ComparisonTie
	}
	return nil
}
