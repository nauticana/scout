package evaluation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func staticExecutor(outputs map[domain.EvaluationRole]string) fake.CaseExecutorFunc {
	return func(_ context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, preserved []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		role := domain.RoleBaseline
		if subject.AgentVersion == "v2" {
			role = domain.RoleCandidate
		}
		retrieval := preserved
		if retrieval == nil {
			retrieval = []domain.KnowledgeMatch{{DocumentID: "d-" + string(role), ChunkID: "c1", Content: []byte("evidence")}}
		}
		return domain.EvaluationCase{Role: role, Output: []byte(outputs[role]), Retrieval: retrieval, Latency: 10 * time.Millisecond}, nil
	}
}

func judgeOrNil(judge *fake.JudgeEvaluator) contract.JudgeEvaluator {
	if judge == nil {
		return nil
	}
	return judge
}

func testRunner(executor fake.CaseExecutorFunc, judge *fake.JudgeEvaluator, queue *MemoryReviewQueue) *Runner {
	return &Runner{
		Executor: executor,
		Judge:    judgeOrNil(judge),
		Review:   queue,
		Scorer:   &PairedScorer{Seed: 9, Resamples: 100, Policy: domain.PromotionPolicy{MinSamples: 1, Tolerances: map[string]float64{domain.MetricCorrectness: 0.5}}},
		Now:      fixedClock(testClock),
	}
}

func TestRunnerPreservesBaselineRetrievalForTheCandidate(t *testing.T) {
	var mu sync.Mutex
	var preservedSeen []bool
	executor := fake.CaseExecutorFunc(func(_ context.Context, subject domain.EvaluationSubject, _ domain.GoldenExample, preserved []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		mu.Lock()
		if subject.AgentVersion == "v2" {
			preservedSeen = append(preservedSeen, preserved != nil)
		}
		mu.Unlock()
		return domain.EvaluationCase{Output: []byte("out"), Retrieval: []domain.KnowledgeMatch{{DocumentID: "d1", ChunkID: "c1"}}}, nil
	})
	examples := []domain.GoldenExample{testExample("a"), testExample("b")}
	manifest := testManifest(t, examples...)
	runner := testRunner(executor, nil, &MemoryReviewQueue{Now: fixedClock(testClock)})

	if _, err := runner.Run(context.Background(), manifest, examples); err != nil {
		t.Fatal(err)
	}
	if len(preservedSeen) != 2 || !preservedSeen[0] || !preservedSeen[1] {
		t.Fatalf("candidate did not reuse the baseline retrieval: %v", preservedSeen)
	}

	// A candidate that changes the index must retrieve for itself.
	changed := manifest
	changed.Candidate.IndexGeneration = 9
	rebuilt, err := (&ManifestBuilder{Now: fixedClock(testClock)}).Build(changed)
	if err != nil {
		t.Fatal(err)
	}
	preservedSeen = nil
	if _, err := runner.Run(context.Background(), rebuilt, examples); err != nil {
		t.Fatal(err)
	}
	for _, seen := range preservedSeen {
		if seen {
			t.Fatal("retrieval was preserved across a changed index generation")
		}
	}
}

func TestRunnerRoutesLowConfidenceDisagreementAndHighRiskToReview(t *testing.T) {
	examples := []domain.GoldenExample{testExample("a"), testExample("b"), testExample("c")}
	examples[2].RiskTier = "critical"
	manifest := testManifest(t, examples...)
	queue := &MemoryReviewQueue{Now: fixedClock(testClock)}
	judge := &fake.JudgeEvaluator{
		EvaluatorVersion: domain.EvaluatorVersion{Kind: "judge", Version: "j1"},
		CompareFunc: func(_ context.Context, pair domain.EvaluationPair) (domain.JudgeVerdict, error) {
			switch pair.Example.ExampleID {
			case "a":
				return domain.JudgeVerdict{Confidence: 0.2, Candidate: []domain.EvaluationScore{{Metric: domain.MetricCorrectness, Value: 1, Evaluator: domain.EvaluatorVersion{Kind: "judge", Version: "j1"}}}}, nil
			case "b":
				// Judge contradicts the heuristic on the same metric.
				return domain.JudgeVerdict{Confidence: 1, Candidate: []domain.EvaluationScore{{Metric: domain.MetricCorrectness, Value: 0, Evaluator: domain.EvaluatorVersion{Kind: "judge", Version: "j1"}}}}, nil
			default:
				return domain.JudgeVerdict{Confidence: 1, Candidate: []domain.EvaluationScore{{Metric: domain.MetricCorrectness, Value: 1, Evaluator: domain.EvaluatorVersion{Kind: "judge", Version: "j1"}}}}, nil
			}
		},
	}
	heuristic := &fake.HeuristicEvaluator{
		EvaluatorVersion: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"},
		EvaluateFunc: func(_ context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
			return []domain.EvaluationScore{{Metric: domain.MetricCorrectness, Value: 1, Confidence: 1, Evaluator: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"}}}, nil
		},
	}
	runner := testRunner(staticExecutor(map[domain.EvaluationRole]string{domain.RoleBaseline: "b", domain.RoleCandidate: "c"}), judge, queue)
	runner.Heuristics = []contract.HeuristicEvaluator{heuristic}
	runner.HighRiskTiers = []string{"critical"}

	summary, err := runner.Run(context.Background(), manifest, examples)
	if err != nil {
		t.Fatal(err)
	}
	if summary.HumanReviewPending != 6 {
		t.Fatalf("review pending = %d", summary.HumanReviewPending)
	}
	items, err := queue.Next(context.Background(), manifest.TenantID, 10)
	if err != nil || len(items) != 3 {
		t.Fatalf("queued = %d, %v", len(items), err)
	}
	reasons := map[string]string{}
	for _, item := range items {
		reasons[item.ExampleID] = item.Reason
	}
	if !strings.Contains(reasons["a"], "confidence") || !strings.Contains(reasons["b"], "disagree") || !strings.Contains(reasons["c"], "high risk") {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestRunnerCachesJudgeInputsByDigest(t *testing.T) {
	examples := []domain.GoldenExample{testExample("a"), testExample("b")}
	manifest := testManifest(t, examples...)
	judge := &fake.JudgeEvaluator{EvaluatorVersion: domain.EvaluatorVersion{Kind: "judge", Version: "j1"},
		CompareFunc: func(context.Context, domain.EvaluationPair) (domain.JudgeVerdict, error) {
			return domain.JudgeVerdict{Confidence: 1}, nil
		}}
	// Both examples produce identical blinded inputs except the example id, so
	// only a replay of the same example may hit the cache.
	runner := testRunner(staticExecutor(map[domain.EvaluationRole]string{domain.RoleBaseline: "same", domain.RoleCandidate: "same"}), judge, &MemoryReviewQueue{Now: fixedClock(testClock)})
	if _, err := runner.Run(context.Background(), manifest, examples); err != nil {
		t.Fatal(err)
	}
	if judge.Calls != 2 {
		t.Fatalf("judge calls on first run = %d", judge.Calls)
	}
	if _, err := runner.Run(context.Background(), manifest, examples); err != nil {
		t.Fatal(err)
	}
	if judge.Calls != 2 {
		t.Fatalf("replay re-invoked the judge: %d calls", judge.Calls)
	}
}

func TestRunnerStopsEarlyWhenTheEffectIsDecided(t *testing.T) {
	var examples []domain.GoldenExample
	for i := range 8 {
		examples = append(examples, testExample(fmt.Sprintf("e%d", i)))
	}
	manifest := testManifest(t, examples...)
	var mu sync.Mutex
	executed := map[string]bool{}
	executor := fake.CaseExecutorFunc(func(_ context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, _ []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		mu.Lock()
		executed[example.ExampleID] = true
		mu.Unlock()
		value := "bad"
		if subject.AgentVersion == "v2" {
			value = "good"
		}
		return domain.EvaluationCase{Output: []byte(value)}, nil
	})
	heuristic := &fake.HeuristicEvaluator{EvaluatorVersion: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"},
		EvaluateFunc: func(_ context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
			value := 0.0
			if string(evalCase.Output) == "good" {
				value = 1
			}
			return []domain.EvaluationScore{{Metric: domain.MetricCorrectness, Value: value, Confidence: 1, Evaluator: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"}}}, nil
		}}
	runner := testRunner(executor, nil, &MemoryReviewQueue{Now: fixedClock(testClock)})
	runner.Heuristics = []contract.HeuristicEvaluator{heuristic}
	runner.EarlyStopBatch = 2
	runner.Scorer = &PairedScorer{Seed: 4, Resamples: 200, Policy: domain.PromotionPolicy{MinSamples: 2, Tolerances: map[string]float64{domain.MetricCorrectness: 0.01}}}

	summary, err := runner.Run(context.Background(), manifest, examples)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.StoppedEarly || summary.Samples >= len(examples) {
		t.Fatalf("summary = %+v", summary)
	}
	if len(executed) != summary.Samples {
		t.Fatalf("executed %d examples for %d samples", len(executed), summary.Samples)
	}
}

func TestRunnerRecordsRunLifecycleAndFailure(t *testing.T) {
	examples := []domain.GoldenExample{testExample("a")}
	manifest := testManifest(t, examples...)
	var statuses []string
	store := &fake.EvaluationResultStore{
		StartRunFunc: func(_ context.Context, run domain.EvaluationRun) (int64, error) {
			statuses = append(statuses, run.Status)
			return 42, nil
		},
		FinishRunFunc: func(_ context.Context, run domain.EvaluationRun) error {
			statuses = append(statuses, run.Status)
			return nil
		},
		PutResultsFunc: func(_ context.Context, tenantID int64, runID int64, results []domain.EvaluationResult) error {
			if runID != 42 || tenantID != manifest.TenantID || len(results) != 2 {
				return fmt.Errorf("unexpected results: %d %d %d", tenantID, runID, len(results))
			}
			return nil
		},
	}
	runner := testRunner(staticExecutor(map[domain.EvaluationRole]string{domain.RoleBaseline: "b", domain.RoleCandidate: "c"}), nil, &MemoryReviewQueue{Now: fixedClock(testClock)})
	runner.Results = store
	if _, err := runner.Run(context.Background(), manifest, examples); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0] != runStatusRunning || statuses[1] != runStatusCompleted {
		t.Fatalf("statuses = %v", statuses)
	}

	failing := testRunner(fake.CaseExecutorFunc(func(context.Context, domain.EvaluationSubject, domain.GoldenExample, []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		return domain.EvaluationCase{}, errors.New("provider down")
	}), nil, &MemoryReviewQueue{Now: fixedClock(testClock)})
	statuses = nil
	failing.Results = store
	if _, err := failing.Run(context.Background(), manifest, examples); err == nil {
		t.Fatal("executor failure did not surface")
	}
	if len(statuses) != 2 || statuses[1] != runStatusFailed {
		t.Fatalf("failure statuses = %v", statuses)
	}
}

func TestRunnerValidation(t *testing.T) {
	examples := []domain.GoldenExample{testExample("a")}
	manifest := testManifest(t, examples...)
	runner := testRunner(staticExecutor(nil), nil, &MemoryReviewQueue{})
	if _, err := runner.Run(context.Background(), manifest, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no examples = %v", err)
	}
	if _, err := runner.Run(context.Background(), manifest, []domain.GoldenExample{examples[0], examples[0]}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("duplicate example = %v", err)
	}
	foreign := testExample("a")
	foreign.TenantID = 99
	if _, err := runner.Run(context.Background(), manifest, []domain.GoldenExample{foreign}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign tenant = %v", err)
	}
	tampered := manifest
	tampered.SafetyPolicyVersion = "other"
	if _, err := runner.Run(context.Background(), tampered, examples); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("tampered manifest = %v", err)
	}
}
