package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func trajectoryCase(t *testing.T, expectation TrajectoryExpectation, events []domain.TrajectoryEvent) domain.EvaluationCase {
	t.Helper()
	example := testExample("t1")
	encoded, err := json.Marshal(expectation)
	if err != nil {
		t.Fatal(err)
	}
	example.ExpectedBehavior = encoded
	return domain.EvaluationCase{Example: example, Role: domain.RoleCandidate, Trajectory: events}
}

func scoreByMetric(scores []domain.EvaluationScore) map[string]domain.EvaluationScore {
	byMetric := make(map[string]domain.EvaluationScore, len(scores))
	for _, score := range scores {
		byMetric[score.Metric] = score
	}
	return byMetric
}

func TestTrajectoryScorerScoresPlanComplianceAndEfficiency(t *testing.T) {
	expectation := TrajectoryExpectation{
		Tools: []string{"lookup", "refund"}, Ordered: true, Arguments: map[string][]string{"refund": {"order_id"}},
		ForbiddenTools: []string{"delete_account"}, OptimalSteps: 4, FinalState: "settled",
	}
	events := []domain.TrajectoryEvent{
		{Sequence: 1, Kind: domain.TrajectoryToolCall, Name: "lookup", Payload: []byte(`{"q":"x"}`)},
		{Sequence: 2, Kind: domain.TrajectoryObservation, Name: "lookup"},
		{Sequence: 3, Kind: domain.TrajectoryToolCall, Name: "refund", Payload: []byte(`{"order_id":"o1"}`)},
		{Sequence: 4, Kind: domain.TrajectoryObservation, Name: "refund"},
		{Sequence: 5, Kind: domain.TrajectoryState, Name: "settled"},
	}
	scorer := &TrajectoryScorer{Revision: "traj-1"}
	scores, err := scorer.Score(context.Background(), trajectoryCase(t, expectation, events))
	if err != nil {
		t.Fatal(err)
	}
	byMetric := scoreByMetric(scores)
	for _, metric := range []string{domain.MetricToolChoice, domain.MetricToolArguments, domain.MetricPolicyCompliance, domain.MetricTrajectoryEfficiency, domain.MetricRecoverability, domain.MetricFinalState, domain.MetricPartialSafety} {
		if byMetric[metric].Value != 1 {
			t.Fatalf("%s = %+v", metric, byMetric[metric])
		}
	}
	if byMetric[domain.MetricToolChoice].Evaluator.Kind != evaluatorKindTrajectory {
		t.Fatalf("evaluator = %+v", byMetric[domain.MetricToolChoice].Evaluator)
	}
}

func TestTrajectoryScorerPenalizesViolationsAndInefficiency(t *testing.T) {
	expectation := TrajectoryExpectation{Tools: []string{"lookup", "refund"}, ForbiddenTools: []string{"delete_account"}, OptimalSteps: 2, FinalState: "settled"}
	events := []domain.TrajectoryEvent{
		{Sequence: 1, Kind: domain.TrajectoryToolCall, Name: "lookup"},
		{Sequence: 2, Kind: domain.TrajectoryToolCall, Name: "delete_account"},
		{Sequence: 3, Kind: domain.TrajectoryObservation, Name: "delete_account", Failed: true},
		{Sequence: 4, Kind: domain.TrajectoryToolCall, Name: "lookup", Recovered: true},
		{Sequence: 5, Kind: domain.TrajectoryState, Name: "aborted"},
	}
	scores, err := (&TrajectoryScorer{Revision: "traj-1"}).Score(context.Background(), trajectoryCase(t, expectation, events))
	if err != nil {
		t.Fatal(err)
	}
	byMetric := scoreByMetric(scores)
	if byMetric[domain.MetricPolicyCompliance].Value != 0 || !byMetric[domain.MetricPolicyCompliance].Critical {
		t.Fatalf("forbidden tool not a critical failure: %+v", byMetric[domain.MetricPolicyCompliance])
	}
	if byMetric[domain.MetricToolChoice].Value != 0.5 || byMetric[domain.MetricFinalState].Value != 0 {
		t.Fatalf("choice/final = %+v", byMetric)
	}
	if byMetric[domain.MetricTrajectoryEfficiency].Value != 0.5 {
		t.Fatalf("efficiency = %+v", byMetric[domain.MetricTrajectoryEfficiency])
	}
	if byMetric[domain.MetricRecoverability].Value != 1 {
		t.Fatalf("recoverability = %+v", byMetric[domain.MetricRecoverability])
	}
}

func TestTrajectoryScorerScoresStreamingSignals(t *testing.T) {
	expectation := TrajectoryExpectation{Interruptible: true}
	events := []domain.TrajectoryEvent{
		{Sequence: 1, Kind: domain.TrajectoryToken, Offset: 200 * time.Millisecond},
		{Sequence: 2, Kind: domain.TrajectoryPolicy, Name: "interrupt"},
		{Sequence: 3, Kind: domain.TrajectoryState, Name: "stopped"},
	}
	scorer := &TrajectoryScorer{Revision: "traj-1", TimeToFirstBudget: 300 * time.Millisecond}
	byMetric := scoreByMetric(mustScore(t, scorer, trajectoryCase(t, expectation, events)))
	if byMetric[domain.MetricTimeToFirstMs].Value != 1 || byMetric[domain.MetricInterruptibility].Value != 1 {
		t.Fatalf("streaming = %+v", byMetric)
	}

	unsafe := []domain.TrajectoryEvent{
		{Sequence: 1, Kind: domain.TrajectoryToken, Offset: 900 * time.Millisecond, Failed: true},
		{Sequence: 2, Kind: domain.TrajectoryPolicy, Name: "interrupt"},
	}
	byMetric = scoreByMetric(mustScore(t, scorer, trajectoryCase(t, expectation, unsafe)))
	if byMetric[domain.MetricTimeToFirstMs].Value != 0 || byMetric[domain.MetricInterruptibility].Value != 0 {
		t.Fatalf("late/uninterrupted = %+v", byMetric)
	}
	if byMetric[domain.MetricPartialSafety].Value != 0 || !byMetric[domain.MetricPartialSafety].Critical {
		t.Fatalf("partial safety = %+v", byMetric[domain.MetricPartialSafety])
	}
}

func mustScore(t *testing.T, scorer *TrajectoryScorer, evalCase domain.EvaluationCase) []domain.EvaluationScore {
	t.Helper()
	scores, err := scorer.Score(context.Background(), evalCase)
	if err != nil {
		t.Fatal(err)
	}
	return scores
}

func TestRecordingSandboxIsDeterministicAndBounded(t *testing.T) {
	ctx := context.Background()
	sandbox := &RecordingSandbox{Responses: map[string]domain.ToolResult{"lookup": {Output: []byte("hit")}}, Strict: true, MaxCalls: 2}
	call := domain.ToolCall{TenantContext: domain.TenantContext{TenantID: 7}, ToolID: "lookup", Arguments: []byte(`{"q":"x"}`)}

	first, err := sandbox.Invoke(ctx, call)
	if err != nil || string(first.Output) != "hit" {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := sandbox.Invoke(ctx, call)
	if err != nil || string(second.Output) != string(first.Output) {
		t.Fatalf("replay is not deterministic: %+v, %v", second, err)
	}
	if _, err := sandbox.Invoke(ctx, call); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("call limit = %v", err)
	}
	if recorded := sandbox.Recorded(); len(recorded) != 2 || recorded[0].ToolID != "lookup" {
		t.Fatalf("recorded = %+v", recorded)
	}
	sandbox.Reset()
	if len(sandbox.Recorded()) != 0 {
		t.Fatal("reset did not clear recorded calls")
	}
	if _, err := sandbox.Invoke(ctx, domain.ToolCall{TenantContext: domain.TenantContext{TenantID: 7}, ToolID: "unknown"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("strict unknown tool = %v", err)
	}
}
