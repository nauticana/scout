package evaluation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var judgeVersion = domain.EvaluatorVersion{Kind: "rubric_judge", Version: "judge-co/grader/1@sha256:abc"}

// judgeWith grades every example with spend, failing with gradeErr when set.
func judgeWith(spend domain.Usage, unpriced bool, gradeErr error) (*fake.RubricJudge, *int) {
	var mu sync.Mutex
	graded := 0
	return &fake.RubricJudge{EvaluatorVersion: judgeVersion, GradeFunc: func(_ context.Context, principal domain.PrincipalRef, evalCase domain.EvaluationCase) (domain.RubricVerdict, error) {
		if principal != (domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}) {
			return domain.RubricVerdict{}, fmt.Errorf("%w: graded as %+v, not the run's requester", domain.ErrForbidden, principal)
		}
		mu.Lock()
		graded++
		mu.Unlock()
		verdict := domain.RubricVerdict{Usage: spend, Unpriced: unpriced}
		if gradeErr != nil {
			return verdict, gradeErr
		}
		verdict.Scores = []domain.EvaluationScore{
			{Metric: domain.MetricRubricScore, Value: 0.75, Confidence: 1, Evaluator: judgeVersion, Rationale: "grade 4 of 1..5: " + evalCase.Example.ExampleID},
			{Metric: domain.MetricRubricPass, Value: 1, Confidence: 1, Evaluator: judgeVersion},
		}
		return verdict, nil
	}}, &graded
}

func TestAgentEvaluationWorkerAddsTheJudgesGradeAndSpend(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, _ := draftWorker(t, queue, 5, "EUR")
	judgeModel := domain.ModelReference{ProviderID: "judge-co", ModelID: "grader"}
	judge, graded := judgeWith(domain.Usage{InputTokens: 40, OutputTokens: 4, CostMinorUnits: 7, Currency: "EUR",
		Models: []domain.ModelSpend{{Model: judgeModel, InputTokens: 40, OutputTokens: 4, CostMinorUnits: 7}}}, false, nil)
	w.Judge = judge
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if *graded != 3 || len(queue.recorded) != 3 || queue.finished[0] != string(domain.EvaluationCompleted)+"|" {
		t.Fatalf("graded %d, recorded %d, finished %v", *graded, len(queue.recorded), queue.finished)
	}
	for _, result := range queue.recorded {
		if len(result.Scores) != 3 || result.Scores[0].Evaluator.Version != "h1" || result.Scores[1].Metric != domain.MetricRubricScore ||
			result.Scores[1].Evaluator != judgeVersion || result.Scores[2].Metric != domain.MetricRubricPass {
			t.Fatalf("scores = %+v, want the heuristic's then the judge's", result.Scores)
		}
		if usage := result.Usage; usage.CostMinorUnits != 5 || usage.Currency != "EUR" || usage.InputTokens != 3 || usage.OutputTokens != 0 {
			t.Fatalf("subject usage = %+v, want only the draft's 5 EUR", usage)
		}
		judged := result.EvaluatorUsage
		if judged.CostMinorUnits != 7 || judged.Currency != "EUR" || judged.InputTokens != 40 || judged.OutputTokens != 4 ||
			len(judged.Models) != 1 || judged.Models[0].Model != judgeModel {
			t.Fatalf("evaluator usage = %+v, want the judge's 7 EUR apart from the subject's", judged)
		}
	}
}

func TestAgentEvaluationWorkerCountsTheJudgesSpendAgainstTheCeiling(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, requests := draftWorker(t, queue, 1, "EUR")
	w.MaxRunCostMinorUnits = 10
	w.Judge, _ = judgeWith(domain.Usage{CostMinorUnits: 5, Currency: "EUR"}, false, nil)
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationStoppedEarly)+"|cost ceiling") {
		t.Fatalf("ran %d examples, finished %v; the judge's spend must stop the run after two", len(*requests), queue.finished)
	}
}

func TestAgentEvaluationWorkerFailsTheRunOnAJudgeError(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, _ := draftWorker(t, queue, 5, "EUR")
	w.Judge, _ = judgeWith(domain.Usage{InputTokens: 40, CostMinorUnits: 7, Currency: "EUR"}, false,
		fmt.Errorf("%w: rubric score outside 1..5", domain.ErrInvalidModelOutput))
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.recorded) == 0 || queue.finished[0] != string(domain.EvaluationFailed)+"|example a failed: invalid_output" {
		t.Fatalf("recorded %d, finished %v", len(queue.recorded), queue.finished)
	}
	result := queue.recorded[0]
	if len(result.Scores) != 0 || result.Reason != "judging failed: invalid_output" || result.Usage.CostMinorUnits != 5 ||
		result.EvaluatorUsage.CostMinorUnits != 7 || result.EvaluatorUsage.InputTokens != 40 {
		t.Fatalf("result = %+v, want no scores and the judge's spend settled", result)
	}

	hidden := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	hidden.claim.Run.Scope = domain.GoldenScopeGate
	w, _ = draftWorker(t, hidden, 5, "EUR")
	secret := inputExample("a")
	secret.ExampleID, secret.Hidden = "secret-probe", true
	w.Examples = &examplesFake{examples: []domain.GoldenExample{secret}}
	w.Judge, _ = judgeWith(domain.Usage{}, false, fmt.Errorf("%w: verdict quoted secret-probe", domain.ErrInvalidModelOutput))
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if reason := hidden.finished[0]; reason != string(domain.EvaluationFailed)+"|a gate example failed: invalid_output" {
		t.Fatalf("failure reason %q must not name the hidden example or judge text", reason)
	}
}

func TestAgentEvaluationWorkerFailsClosedOnJudgeSpendItCannotCount(t *testing.T) {
	for name, judge := range map[string]func() (*fake.RubricJudge, *int){
		"unpriced": func() (*fake.RubricJudge, *int) { return judgeWith(domain.Usage{InputTokens: 40}, true, nil) },
		"other currency": func() (*fake.RubricJudge, *int) {
			return judgeWith(domain.Usage{InputTokens: 40, CostMinorUnits: 7, Currency: "USD"}, false, nil)
		},
		"negative": func() (*fake.RubricJudge, *int) { return judgeWith(domain.Usage{InputTokens: -40}, false, nil) },
	} {
		queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
		w, _ := draftWorker(t, queue, 5, "EUR")
		w.Judge, _ = judge()
		if err := w.Execute(context.Background(), testLease); err != nil {
			t.Fatal(err)
		}
		if queue.finished[0] != string(domain.EvaluationFailed)+"|example a failed: validation; its usage was not settled" {
			t.Fatalf("%s: finished %v", name, queue.finished)
		}
		result := queue.recorded[0]
		if len(result.Scores) != 0 || result.Usage.CostMinorUnits != 5 || result.Usage.Currency != "EUR" ||
			result.EvaluatorUsage.CostMinorUnits != 0 || result.EvaluatorUsage.Currency != "" {
			t.Fatalf("%s: result %+v must keep only the countable cost", name, result)
		}
		if want := map[bool]int64{true: 0, false: 40}[name == "negative"]; result.EvaluatorUsage.InputTokens != want {
			t.Fatalf("%s: judge tokens = %+v, want %d", name, result.EvaluatorUsage, want)
		}
	}
}

func TestAgentEvaluationWorkerRefusesAnExampleOfAnotherTenant(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, requests := draftWorker(t, queue, 5, "EUR")
	foreign := inputExample("a")
	foreign.TenantID = 8
	w.Examples = &examplesFake{examples: []domain.GoldenExample{foreign}}
	judge, graded := judgeWith(domain.Usage{}, false, nil)
	w.Judge = judge
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 0 || *graded != 0 || queue.finished[0] != string(domain.EvaluationFailed)+"|example a failed: forbidden" {
		t.Fatalf("ran %d, graded %d, finished %v", len(*requests), *graded, queue.finished)
	}
}

func TestAgentEvaluationWorkerFailsARunWhoseJudgingWasCancelled(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, requests := draftWorker(t, queue, 5, "EUR")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Judge = &fake.RubricJudge{EvaluatorVersion: judgeVersion, GradeFunc: func(ctx context.Context, _ domain.PrincipalRef, _ domain.EvaluationCase) (domain.RubricVerdict, error) {
		cancel()
		return domain.RubricVerdict{Usage: domain.Usage{InputTokens: 40, CostMinorUnits: 7, Currency: "EUR"}}, fmt.Errorf("rubric judge: %w", ctx.Err())
	}}
	if err := w.Execute(ctx, testLease); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled execute = %v, want the run left to the lease's reclaim", err)
	}
	if len(queue.recorded) != 1 || len(queue.finished) != 0 {
		t.Fatalf("recorded %d, finished %v", len(queue.recorded), queue.finished)
	}
	unscored := queue.recorded[0]
	if unscored.Reason != "judging failed: canceled" || len(unscored.Scores) != 0 || unscored.EvaluatorUsage.CostMinorUnits != 7 {
		t.Fatalf("cancelled result = %+v, want its spend settled and the failure recorded", unscored)
	}

	// The reclaimed attempt sees the example as recorded and unscored, as the store loads it.
	reclaimed := draftClaim(map[string]domain.Usage{"a": {InputTokens: 43, CostMinorUnits: 12, Currency: "EUR"}})
	reclaimed.Unscored = map[string]string{"a": unscored.Reason}
	queue.claim, queue.recorded = reclaimed, nil
	w.Judge, _ = judgeWith(domain.Usage{}, false, nil)
	ran := len(*requests)
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.finished) != 1 || queue.finished[0] != string(domain.EvaluationFailed)+"|example a failed: judging failed: canceled" {
		t.Fatalf("reclaimed run finished %v; an unscored example must not leave it completed", queue.finished)
	}
	if len(*requests) != ran || len(queue.recorded) != 0 {
		t.Fatalf("the reclaimed run executed %d more examples", len(*requests)-ran)
	}

	hidden := draftClaim(map[string]domain.Usage{"secret-probe": {}})
	hidden.Unscored = map[string]string{"secret-probe": "scoring failed: canceled"}
	secret := inputExample("a")
	secret.ExampleID, secret.Hidden = "secret-probe", true
	gate := &queueFake{claim: hidden}
	w, _ = draftWorker(t, gate, 5, "EUR")
	w.Examples = &examplesFake{examples: []domain.GoldenExample{secret}}
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if gate.finished[0] != string(domain.EvaluationFailed)+"|a gate example failed: scoring failed: canceled" {
		t.Fatalf("hidden unscored example finished %v", gate.finished)
	}
}

func TestAgentEvaluationWorkerRefusesToJudgeWithoutTheRequester(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	queue.claim.Run.RequestedBy = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "10"}
	w, _ := draftWorker(t, queue, 5, "EUR")
	w.Drafts = fake.DraftTester{ExecuteFunc: func(context.Context, domain.StudioActor, domain.AgentTestRequest, domain.AgentDefinition) (domain.AgentTestResult, error) {
		return domain.AgentTestResult{Output: "answer"}, nil
	}}
	judge, graded := judgeWith(domain.Usage{}, false, nil)
	w.Judge = judge
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if *graded != 0 || queue.finished[0] != string(domain.EvaluationFailed)+"|example a failed: forbidden" {
		t.Fatalf("graded %d, finished %v; the judge must be charged to the run's requester", *graded, queue.finished)
	}
}

// A case stopped by its limit or budget leaves no answer to grade, so the judge is not paid for it.
func TestAgentEvaluationWorkerDoesNotJudgeAStoppedCase(t *testing.T) {
	judge, graded := judgeWith(domain.Usage{CostMinorUnits: 5, Currency: "EUR"}, false, nil)
	w := &AgentEvaluationWorker{Judge: judge, Currency: "EUR"}
	requester := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}
	stoppedCase := domain.EvaluationCase{Trajectory: []domain.TrajectoryEvent{{Kind: domain.TrajectoryState, Name: "execution_limit", Failed: true}}}
	result, err := w.score(context.Background(), requester, domain.GoldenExample{ExampleID: "slow"}, stoppedCase)
	if err != nil || *graded != 0 || len(result.Scores) != 0 || result.EvaluatorUsage.CostMinorUnits != 0 {
		t.Fatalf("result = %+v, graded %d, error = %v", result, *graded, err)
	}
	answered := domain.EvaluationCase{Output: []byte("done"), Trajectory: []domain.TrajectoryEvent{{Kind: domain.TrajectoryToolCall, Name: "search", Failed: true}}}
	if _, err = w.score(context.Background(), requester, domain.GoldenExample{ExampleID: "recovered"}, answered); err != nil || *graded != 1 {
		t.Fatalf("a recovered tool failure must still be judged: graded %d, error = %v", *graded, err)
	}
}
