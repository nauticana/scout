package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// Arm a's spend is settled when arm b then fails, so a reclaimed attempt never pays it again.
func TestAgentEvaluationWorkerSettlesArmAWhenArmBFails(t *testing.T) {
	queue := &queueFake{claim: comparisonClaim(nil)}
	w, outputs, pairs := comparisonWorker(t, queue, nil)
	w.Drafts = fake.DraftTester{ExecuteFunc: func(context.Context, domain.StudioActor, domain.AgentTestRequest, domain.AgentDefinition) (domain.AgentTestResult, error) {
		return domain.AgentTestResult{}, errors.New("provider unavailable")
	}}
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.compared) != 1 || len(*pairs) != 0 || len(outputs.Objects) != 0 {
		t.Fatalf("compared %d, judged %d, stored %d", len(queue.compared), len(*pairs), len(outputs.Objects))
	}
	got := queue.compared[0]
	if got.A.Usage.CostMinorUnits != 2 || got.B.Usage.CostMinorUnits != 0 || got.Verdict != "" || !strings.HasPrefix(got.Reason, "arm b failed") {
		t.Fatalf("settled = %+v", got)
	}
	if !strings.HasPrefix(queue.finished[0], string(domain.EvaluationFailed)+"|example a failed") {
		t.Fatalf("finished = %v", queue.finished)
	}
}

// A cancellation between the arms settles arm a and leaves the run to its reclaim.
func TestAgentEvaluationWorkerSettlesArmAWhenCancelledBetweenArms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := &queueFake{claim: comparisonClaim(nil)}
	w, _, _ := comparisonWorker(t, queue, nil)
	w.Releases = fake.CaseExecutorFunc(func(context.Context, domain.EvaluationSubject, domain.GoldenExample, []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		cancel()
		return domain.EvaluationCase{Output: []byte("release"), Usage: domain.Usage{CostMinorUnits: 2, Currency: "EUR"}}, nil
	})
	ran := false
	w.Drafts = fake.DraftTester{ExecuteFunc: func(context.Context, domain.StudioActor, domain.AgentTestRequest, domain.AgentDefinition) (domain.AgentTestResult, error) {
		ran = true
		return domain.AgentTestResult{}, nil
	}}
	if err := w.Execute(ctx, testLease); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v, want the cancellation", err)
	}
	if ran || len(queue.compared) != 1 || queue.compared[0].A.Usage.CostMinorUnits != 2 || !strings.HasPrefix(queue.compared[0].Reason, "arm b did not run") ||
		len(queue.finished) != 0 {
		t.Fatalf("ran b %v, compared %+v, finished %v", ran, queue.compared, queue.finished)
	}
}

// Answers that cannot be stored still have both arms' spend settled.
func TestAgentEvaluationWorkerSettlesSpendWhenAnswersCannotBeStored(t *testing.T) {
	queue := &queueFake{claim: comparisonClaim(nil)}
	w, outputs, pairs := comparisonWorker(t, queue, nil)
	outputs.PutFunc = func(context.Context, string, []byte) error { return errors.New("bucket unavailable") }
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.compared) != 1 || len(*pairs) != 0 {
		t.Fatalf("compared %d, judged %d", len(queue.compared), len(*pairs))
	}
	got := queue.compared[0]
	if got.A.Usage.CostMinorUnits != 2 || got.B.Usage.CostMinorUnits != 5 || got.A.Output.URI != "" || got.Verdict != "" ||
		!strings.HasPrefix(got.Reason, "storing answers failed") || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationFailed)+"|") {
		t.Fatalf("settled = %+v, finished %v", got, queue.finished)
	}
}

// A judge call cut off by shutdown records nothing, so the reclaimed attempt compares again.
func TestAgentEvaluationWorkerRecordsNoJudgingFailureOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := &queueFake{claim: comparisonClaim(nil)}
	w, _, _ := comparisonWorker(t, queue, func(domain.EvaluationPair) (domain.JudgeVerdict, error) {
		cancel()
		return domain.JudgeVerdict{}, context.Canceled
	})
	if err := w.Execute(ctx, testLease); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v, want the cancellation", err)
	}
	if len(queue.compared) != 0 || len(queue.finished) != 0 {
		t.Fatalf("compared %+v, finished %v", queue.compared, queue.finished)
	}
}
