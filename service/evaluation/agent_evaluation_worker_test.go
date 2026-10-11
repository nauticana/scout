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
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/runtime"
	"github.com/nauticana/scout/service/scope"
)

type queueFake struct {
	mu        sync.Mutex
	claim     domain.AgentEvaluationClaim
	recorded  []domain.EvaluationResult
	compared  []domain.CaseComparison
	recordErr error
	finished  []string
}

func (queue *queueFake) Load(_ context.Context, lease domain.AgentEvaluationLease) (domain.AgentEvaluationClaim, error) {
	claim := queue.claim
	claim.Lease = lease
	return claim, nil
}

func (queue *queueFake) Record(_ context.Context, _ domain.AgentEvaluationLease, result domain.EvaluationResult) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.recordErr != nil {
		return queue.recordErr
	}
	queue.recorded = append(queue.recorded, result)
	return nil
}

func (queue *queueFake) RecordComparison(_ context.Context, _ domain.AgentEvaluationLease, comparison domain.CaseComparison) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.recordErr != nil {
		return queue.recordErr
	}
	queue.compared = append(queue.compared, comparison)
	return nil
}

func (queue *queueFake) Finish(_ context.Context, _ domain.AgentEvaluationLease, status domain.EvaluationRunStatus, reason string) error {
	queue.finished = append(queue.finished, string(status)+"|"+reason)
	return nil
}

var _ contract.AgentEvaluationQueue = (*queueFake)(nil)

type examplesFake struct {
	contract.GoldenSetStore
	examples []domain.GoldenExample
	scope    domain.GoldenScope
}

func (store *examplesFake) ListExamples(_ context.Context, scope domain.GoldenScope, _ int64, _ string, _ int64) ([]domain.GoldenExample, error) {
	store.scope = scope
	return store.examples, nil
}

var inputs = map[string]string{"a": "first question", "b": "second question", "c": "third question"}

func inputExample(id string) domain.GoldenExample {
	example := testExample(id)
	example.Payload = domain.ObjectRef{URI: "object://cases/" + id, Digest: sha256Hex([]byte(inputs[id]))}
	return example
}

func draftWorker(t *testing.T, queue *queueFake, cost int64, currency string) (*AgentEvaluationWorker, *[]domain.AgentTestRequest) {
	t.Helper()
	w, err := NewAgentEvaluationWorker(90*time.Second, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []domain.AgentTestRequest
	w.Queue = queue
	w.Examples = &examplesFake{examples: []domain.GoldenExample{inputExample("a"), inputExample("b"), inputExample("c")}}
	w.Payloads = objectReaderFunc(func(_ context.Context, ref domain.ObjectRef) ([]byte, error) {
		return []byte(inputs[strings.TrimPrefix(ref.URI, "object://cases/")]), nil
	})
	w.Drafts = fake.DraftTester{ExecuteFunc: func(_ context.Context, actor domain.StudioActor, request domain.AgentTestRequest, definition domain.AgentDefinition) (domain.AgentTestResult, error) {
		if actor.TenantID != 7 || actor.Principal.ID != "9" || definition.DefinitionDigest != draftDigest {
			t.Errorf("draft tester ran for %+v on %q", actor, definition.DefinitionDigest)
		}
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		return domain.AgentTestResult{Output: "answer to " + request.Task, LatencyMs: 20, Usage: domain.Usage{InputTokens: 3, CostMinorUnits: cost, Currency: currency}}, nil
	}}
	w.Evaluators = []contract.HeuristicEvaluator{&fake.HeuristicEvaluator{
		EvaluatorVersion: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"},
		EvaluateFunc: func(_ context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
			return []domain.EvaluationScore{{Metric: domain.MetricPatternMatch, Value: 1, Evaluator: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"},
				Rationale: "output " + string(evalCase.Output)}}, nil
		},
	}}
	w.MaxRunCostMinorUnits, w.Currency = 1000, "EUR"
	return w, &requests
}

func draftClaim(recorded map[string]domain.Usage) domain.AgentEvaluationClaim {
	definition := domain.AgentDefinition{AgentID: "writer-a", DefinitionDigest: draftDigest, SettingsDigest: scope.SettingsDigest(draftSettings())}
	return domain.AgentEvaluationClaim{
		Run: domain.AgentEvaluationRun{RunID: 11, TenantID: 7, AgentID: "writer-a", GoldenSetID: "core", SetVersion: 3, Scope: domain.GoldenScopeDev,
			Subject:     domain.AgentEvaluationSubject{Kind: domain.SubjectDraft, AgentID: "writer-a", DefinitionDigest: draftDigest},
			RequestedBy: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}, Status: domain.EvaluationRunning},
		Draft: &definition, DraftSettings: draftSettings(), Recorded: recorded,
	}
}

var testLease = domain.AgentEvaluationLease{TenantID: 7, RunID: 11, Token: 99, TTL: 90 * time.Second}

func TestAgentEvaluationWorkerRunsTheDraftOnlyOnUnrecordedExamples(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{"a": {CostMinorUnits: 4, Currency: "EUR"}})}
	w, requests := draftWorker(t, queue, 5, "EUR")
	w.Concurrency = 2
	if err := w.HandleJob(context.Background(), nil, nil, nil, nil, 11, []any{int64(11), int64(7), int64(99)}); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || len(queue.recorded) != 2 {
		t.Fatalf("ran %d and recorded %d examples, want the 2 unrecorded ones", len(*requests), len(queue.recorded))
	}
	for _, request := range *requests {
		if request.AgentID != "writer-a" || request.LanguageCode != "en-US" || (request.Task != inputs["b"] && request.Task != inputs["c"]) ||
			request.Limits != (domain.ExecutionLimits{MaxDuration: 30 * time.Second}) {
			t.Fatalf("request = %+v", request)
		}
	}
	for _, result := range queue.recorded {
		if result.Role != domain.RoleCandidate || result.Usage.CostMinorUnits != 5 || len(result.Scores) != 1 || result.Scores[0].Evaluator.Version != "h1" ||
			result.Latency != 20*time.Millisecond || !strings.HasPrefix(result.Scores[0].Rationale, "output answer to") {
			t.Fatalf("result = %+v", result)
		}
	}
	if scope := w.Examples.(*examplesFake).scope; scope != domain.GoldenScopeDev {
		t.Fatalf("examples read in the %s scope, want the run's", scope)
	}
	if len(queue.finished) != 1 || queue.finished[0] != string(domain.EvaluationCompleted)+"|" {
		t.Fatalf("finished = %v", queue.finished)
	}
}

func TestAgentEvaluationWorkerStopsAtTheCostCeiling(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, requests := draftWorker(t, queue, 6, "EUR")
	w.MaxRunCostMinorUnits = 10
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || len(queue.finished) != 1 || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationStoppedEarly)+"|cost ceiling") {
		t.Fatalf("ran %d, finished %v", len(*requests), queue.finished)
	}

	spent := &queueFake{claim: draftClaim(map[string]domain.Usage{"a": {CostMinorUnits: 10, Currency: "EUR"}})}
	w, requests = draftWorker(t, spent, 6, "EUR")
	w.MaxRunCostMinorUnits = 10
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 0 || spent.finished[0] != string(domain.EvaluationStoppedEarly)+"|cost ceiling of 10 EUR minor units reached" {
		t.Fatalf("a retried run past its ceiling ran %d examples, finished %v", len(*requests), spent.finished)
	}
}

func TestAgentEvaluationWorkerFailsClosed(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, _ := draftWorker(t, queue, 5, "USD")
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.recorded) != 0 || len(queue.finished) != 1 || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationFailed)+"|example a failed: validation") {
		t.Fatalf("a result in another currency: recorded %d, finished %v", len(queue.recorded), queue.finished)
	}

	tampered := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, requests := draftWorker(t, tampered, 5, "EUR")
	w.Payloads = objectReaderFunc(func(context.Context, domain.ObjectRef) ([]byte, error) { return []byte("swapped"), nil })
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 0 || !strings.HasPrefix(tampered.finished[0], string(domain.EvaluationFailed)+"|") {
		t.Fatalf("an input not matching its digest ran %d times, finished %v", len(*requests), tampered.finished)
	}

	hidden := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	hidden.claim.Run.Scope = domain.GoldenScopeGate
	w, _ = draftWorker(t, hidden, 5, "EUR")
	secret := inputExample("a")
	secret.ExampleID, secret.Hidden = "secret-probe", true
	w.Examples = &examplesFake{examples: []domain.GoldenExample{secret}}
	w.Drafts = fake.DraftTester{ExecuteFunc: func(context.Context, domain.StudioActor, domain.AgentTestRequest, domain.AgentDefinition) (domain.AgentTestResult, error) {
		return domain.AgentTestResult{}, errors.Join(domain.ErrRateLimited, errors.New("provider said: quota for key sk-123"))
	}}
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if reason := hidden.finished[0]; reason != string(domain.EvaluationFailed)+"|a gate example failed: rate_limited" {
		t.Fatalf("failure reason %q must name neither the hidden example nor provider text", reason)
	}

	unpriced := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, _ = draftWorker(t, unpriced, 5, "EUR")
	w.MaxRunCostMinorUnits = 0
	if err := w.Execute(context.Background(), testLease); err == nil || len(unpriced.finished) != 0 {
		t.Fatalf("a worker without a cost ceiling ran: %v, %v", err, unpriced.finished)
	}
	if _, err := NewAgentEvaluationWorker(0, 5, 3); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero lease = %v", err)
	}
}

// An executed example's usage is settled even when its scoring fails, and a free
// result in another currency does not fail the run.
func TestAgentEvaluationWorkerSettlesUsageItCannotScore(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, requests := draftWorker(t, queue, 900, "EUR")
	w.MaxRunCostMinorUnits = 10_000
	w.Evaluators = []contract.HeuristicEvaluator{&fake.HeuristicEvaluator{
		EvaluatorVersion: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"},
		EvaluateFunc: func(context.Context, domain.EvaluationCase) ([]domain.EvaluationScore, error) {
			return nil, errors.Join(domain.ErrValidation, errors.New("rubric parse error near secret text"))
		},
	}}
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 1 || len(queue.recorded) != 1 {
		t.Fatalf("ran %d and recorded %d examples, want the failing one settled", len(*requests), len(queue.recorded))
	}
	settled := queue.recorded[0]
	if settled.ExampleID != "a" || settled.Usage.CostMinorUnits != 900 || settled.Scores != nil || settled.Reason != "scoring failed: validation" {
		t.Fatalf("settled result = %+v", settled)
	}
	if len(queue.finished) != 1 || queue.finished[0] != string(domain.EvaluationFailed)+"|example a failed: validation" {
		t.Fatalf("finished = %v", queue.finished)
	}

	free := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, _ = draftWorker(t, free, 0, "USD")
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(free.recorded) != 3 || free.finished[0] != string(domain.EvaluationCompleted)+"|" {
		t.Fatalf("free foreign-currency results: recorded %d, finished %v", len(free.recorded), free.finished)
	}

	refused := &queueFake{claim: draftClaim(map[string]domain.Usage{}), recordErr: domain.ErrValidation}
	w, _ = draftWorker(t, refused, 5, "EUR")
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(refused.finished) != 1 || refused.finished[0] != string(domain.EvaluationFailed)+"|example a failed: validation; its usage was not settled" {
		t.Fatalf("a result the run cannot hold must fail the run, not wait for reclaim: %v", refused.finished)
	}
}

func TestAgentEvaluationWorkerLeavesALostLeaseToReclaim(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{}), recordErr: domain.ErrConflict}
	w, _ := draftWorker(t, queue, 5, "EUR")
	if err := w.Execute(context.Background(), testLease); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("lost lease = %v", err)
	}
	if len(queue.finished) != 0 {
		t.Fatalf("a worker without the lease finished the run: %v", queue.finished)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, _ = draftWorker(t, canceled, 5, "EUR")
	if err := w.Execute(ctx, testLease); err == nil || len(canceled.finished) != 0 {
		t.Fatalf("a canceled worker settled the run: %v, %v", err, canceled.finished)
	}
}

func TestAgentEvaluationWorkerReplaysAReleaseThroughTheCaseExecutor(t *testing.T) {
	claim := draftClaim(map[string]domain.Usage{})
	claim.Draft = nil
	claim.Run.Subject = domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v2", DefinitionDigest: draftDigest}
	queue := &queueFake{claim: claim}
	w, requests := draftWorker(t, queue, 5, "EUR")
	var subjects []domain.EvaluationSubject
	var mu sync.Mutex
	w.Releases = fake.CaseExecutorFunc(func(_ context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, _ []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		mu.Lock()
		subjects = append(subjects, subject)
		mu.Unlock()
		return domain.EvaluationCase{Output: []byte("replayed " + example.ExampleID), Usage: domain.Usage{CostMinorUnits: 1, Currency: "EUR"}}, nil
	})
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 0 || len(subjects) != 3 || subjects[0].AgentID != "writer-a" || subjects[0].AgentVersion != "v2" || queue.finished[0] != string(domain.EvaluationCompleted)+"|" {
		t.Fatalf("draft runs %d, release subjects %+v, finished %v", len(*requests), subjects, queue.finished)
	}

	uncomposed := &queueFake{claim: claim}
	w, _ = draftWorker(t, uncomposed, 5, "EUR")
	if err := w.Execute(context.Background(), testLease); err != nil || uncomposed.finished[0] != string(domain.EvaluationFailed)+"|release evaluation is not composed" {
		t.Fatalf("release run without an executor: %v, %v", err, uncomposed.finished)
	}
}

func durationStop(cost int64) error {
	return &runtime.DraftLimitError{
		Err:   fmt.Errorf("%w: draft exceeded its 30s duration", domain.ErrExecutionLimit),
		Usage: domain.Usage{InputTokens: 40, CostMinorUnits: cost, Currency: "EUR"},
	}
}

func TestAgentEvaluationWorkerScoresADraftStoppedByItsFrozenDuration(t *testing.T) {
	queue := &queueFake{claim: draftClaim(nil)}
	w, _ := draftWorker(t, queue, 5, "EUR")
	w.Examples = &examplesFake{examples: []domain.GoldenExample{inputExample("a")}}
	var trajectory []domain.TrajectoryEvent
	w.Evaluators = []contract.HeuristicEvaluator{&fake.HeuristicEvaluator{
		EvaluatorVersion: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"},
		EvaluateFunc: func(_ context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
			trajectory = evalCase.Trajectory
			return []domain.EvaluationScore{{Metric: domain.MetricPatternMatch, Value: 0, Evaluator: domain.EvaluatorVersion{Kind: "heuristic", Version: "h1"}}}, nil
		},
	}}
	w.Drafts = fake.DraftTester{ExecuteFunc: func(_ context.Context, _ domain.StudioActor, request domain.AgentTestRequest, _ domain.AgentDefinition) (domain.AgentTestResult, error) {
		if request.Limits.MaxDuration != 30*time.Second {
			t.Errorf("limits = %+v, want the pinned draft's", request.Limits)
		}
		return domain.AgentTestResult{}, durationStop(6)
	}}
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.recorded) != 1 || len(queue.finished) != 1 || queue.finished[0] != string(domain.EvaluationCompleted)+"|" ||
		len(trajectory) != 1 || trajectory[0].Name != "execution_limit" || !trajectory[0].Failed {
		t.Fatalf("recorded %d, finished %v, trajectory %+v; the draft's limit is a scored outcome", len(queue.recorded), queue.finished, trajectory)
	}
	if usage := queue.recorded[0].Usage; usage.CostMinorUnits != 6 || usage.InputTokens != 40 || usage.Currency != "EUR" {
		t.Fatalf("recorded usage = %+v, want the stop's settled spend", usage)
	}
}

// Stopped drafts still spend: their settled cost counts toward the run's ceiling.
func TestAgentEvaluationWorkerCountsStoppedDraftsTowardTheCostCeiling(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, _ := draftWorker(t, queue, 0, "EUR")
	ran := 0
	w.Drafts = fake.DraftTester{ExecuteFunc: func(context.Context, domain.StudioActor, domain.AgentTestRequest, domain.AgentDefinition) (domain.AgentTestResult, error) {
		ran++
		return domain.AgentTestResult{}, durationStop(6)
	}}
	w.MaxRunCostMinorUnits = 10
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if ran != 2 || len(queue.finished) != 1 || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationStoppedEarly)+"|cost ceiling") {
		t.Fatalf("ran %d, finished %v; stopped drafts must not run free past the ceiling", ran, queue.finished)
	}
}

func TestAgentEvaluationWorkerFailsAStopThatSettledNoUsage(t *testing.T) {
	queue := &queueFake{claim: draftClaim(nil)}
	w, _ := draftWorker(t, queue, 5, "EUR")
	w.Drafts = fake.DraftTester{ExecuteFunc: func(context.Context, domain.StudioActor, domain.AgentTestRequest, domain.AgentDefinition) (domain.AgentTestResult, error) {
		return domain.AgentTestResult{}, fmt.Errorf("%w: draft exceeded its duration", domain.ErrExecutionLimit)
	}}
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.recorded) != 0 || len(queue.finished) != 1 || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationFailed)+"|") {
		t.Fatalf("recorded %d, finished %v; an unsettled stop must fail the run", len(queue.recorded), queue.finished)
	}
}

func TestAgentEvaluationWorkerFailsADraftRunWhoseLimitsCannotBeRead(t *testing.T) {
	malformed := []domain.EffectiveResource{{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Value: []byte(`{"max_duration_seconds":-1}`)}}
	cases := map[string]func(*domain.AgentEvaluationClaim){
		"settings the definition does not digest": func(claim *domain.AgentEvaluationClaim) {
			claim.DraftSettings = []domain.EffectiveResource{{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Value: []byte(`{"max_duration_seconds":3600}`)}}
		},
		"no pinned settings": func(claim *domain.AgentEvaluationClaim) { claim.DraftSettings = nil },
		"undigested definition": func(claim *domain.AgentEvaluationClaim) {
			claim.Draft.SettingsDigest, claim.DraftSettings = "", nil
		},
		"malformed limits": func(claim *domain.AgentEvaluationClaim) {
			claim.DraftSettings, claim.Draft.SettingsDigest = malformed, scope.SettingsDigest(malformed)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			claim := draftClaim(nil)
			change(&claim)
			queue := &queueFake{claim: claim}
			w, requests := draftWorker(t, queue, 5, "EUR")
			if err := w.Execute(context.Background(), testLease); err != nil {
				t.Fatal(err)
			}
			if len(*requests) != 0 || len(queue.recorded) != 0 || len(queue.finished) != 1 ||
				!strings.HasPrefix(queue.finished[0], string(domain.EvaluationFailed)+"|draft limits cannot be read") {
				t.Fatalf("ran %d, finished %v; the run must fail before any example", len(*requests), queue.finished)
			}
		})
	}
}
