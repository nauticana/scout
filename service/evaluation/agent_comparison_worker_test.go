package evaluation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// comparisonWorker compares the draft (arm b, 5 EUR a case) with release v1 (arm a, 2 EUR a
// case); the pair judge prefers b for 1 EUR unless decide says otherwise.
func comparisonWorker(t *testing.T, queue *queueFake, decide func(domain.EvaluationPair) (domain.JudgeVerdict, error)) (*AgentEvaluationWorker, *fake.ObjectStorage, *[]domain.EvaluationPair) {
	t.Helper()
	w, _ := draftWorker(t, queue, 5, "EUR")
	w.Evaluators = nil
	w.Releases = fake.CaseExecutorFunc(func(_ context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, _ []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		if subject.AgentVersion != "v1" || subject.AgentID != "writer-a" {
			t.Errorf("release executed = %+v", subject)
		}
		return domain.EvaluationCase{Output: []byte("release says " + example.ExampleID), Usage: domain.Usage{CostMinorUnits: 2, Currency: "EUR"}}, nil
	})
	outputs := &fake.ObjectStorage{Name: "out"}
	var mu sync.Mutex
	var pairs []domain.EvaluationPair
	w.Outputs = outputs
	w.PairJudge = &fake.JudgeEvaluator{EvaluatorVersion: domain.EvaluatorVersion{Kind: "judge", Version: "pair@p/m/1"},
		CompareFunc: func(_ context.Context, pair domain.EvaluationPair) (domain.JudgeVerdict, error) {
			mu.Lock()
			pairs = append(pairs, pair)
			mu.Unlock()
			if decide != nil {
				return decide(pair)
			}
			return domain.JudgeVerdict{Preferred: domain.RoleCandidate, Confidence: 0.9, Rationale: "b is better", InputDigest: sha256Hex([]byte(pair.Example.ExampleID)),
				Candidate: []domain.EvaluationScore{{Metric: domain.MetricCorrectness, Value: 1}}, Usage: domain.Usage{InputTokens: 50, CostMinorUnits: 1, Currency: "EUR"}}, nil
		}}
	return w, outputs, &pairs
}

func comparisonClaim(recorded map[string]domain.Usage) domain.AgentEvaluationClaim {
	claim := draftClaim(recorded)
	claim.Run.Baseline = &domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v1", DefinitionDigest: sha256Hex([]byte("v1"))}
	claim.Baseline = &domain.EvaluationArm{Subject: *claim.Run.Baseline}
	return claim
}

func TestAgentEvaluationWorkerComparesBothArmsOfEveryExample(t *testing.T) {
	queue := &queueFake{claim: comparisonClaim(map[string]domain.Usage{"a": {CostMinorUnits: 8, Currency: "EUR"}})}
	w, outputs, pairs := comparisonWorker(t, queue, nil)
	w.Concurrency = 2
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.compared) != 2 || len(queue.recorded) != 0 || len(*pairs) != 2 {
		t.Fatalf("compared %d, recorded %d, judged %d; want the 2 unrecorded examples compared", len(queue.compared), len(queue.recorded), len(*pairs))
	}
	for _, pair := range *pairs {
		if pair.Principal.ID != "9" || !strings.HasPrefix(string(pair.Baseline.Output), "release says") || !strings.HasPrefix(string(pair.Candidate.Output), "answer to") {
			t.Fatalf("pair = %+v", pair)
		}
	}
	for _, comparison := range queue.compared {
		if comparison.Verdict != domain.ComparisonPrefersB || comparison.Evaluator.Version != "pair@p/m/1" || comparison.Rationale != "b is better" ||
			comparison.A.Usage.CostMinorUnits != 2 || comparison.B.Usage.CostMinorUnits != 5 || comparison.EvaluatorUsage.CostMinorUnits != 1 ||
			len(comparison.B.Scores) != 1 || comparison.A.Role != domain.RoleBaseline || comparison.B.Role != domain.RoleCandidate {
			t.Fatalf("comparison = %+v", comparison)
		}
		for _, output := range []domain.ObjectRef{comparison.A.Output, comparison.B.Output} {
			if !strings.HasPrefix(output.URI, "object://out/evaluation/output/7/11/") || !isSHA256Hex(output.Digest) {
				t.Fatalf("output = %+v", output)
			}
		}
	}
	if len(outputs.Objects) != 4 || queue.finished[0] != string(domain.EvaluationCompleted)+"|" {
		t.Fatalf("stored %d answers, finished %v", len(outputs.Objects), queue.finished)
	}
}

// Both arms and the judge count against the ceiling, which an earlier attempt's spend already reached.
func TestAgentEvaluationWorkerStopsAComparisonAtTheCostCeiling(t *testing.T) {
	queue := &queueFake{claim: comparisonClaim(nil)}
	w, _, _ := comparisonWorker(t, queue, nil)
	w.MaxRunCostMinorUnits = 8
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.compared) != 1 || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationStoppedEarly)+"|") {
		t.Fatalf("compared %d, finished %v", len(queue.compared), queue.finished)
	}
}

// An arm that ended in a failed state loses without a judge call; a judging failure is
// recorded with the spend and fails the run without naming a hidden example.
func TestAgentEvaluationWorkerDecidesFailedArmsAndFailsOnJudgingErrors(t *testing.T) {
	queue := &queueFake{claim: comparisonClaim(nil)}
	w, _, pairs := comparisonWorker(t, queue, nil)
	w.Releases = fake.CaseExecutorFunc(func(context.Context, domain.EvaluationSubject, domain.GoldenExample, []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		return domain.EvaluationCase{Trajectory: []domain.TrajectoryEvent{{Kind: domain.TrajectoryState, Name: "execution limit", Failed: true}},
			Usage: domain.Usage{CostMinorUnits: 1, Currency: "EUR"}}, nil
	})
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(*pairs) != 0 || len(queue.compared) != 3 {
		t.Fatalf("judged %d, compared %d", len(*pairs), len(queue.compared))
	}
	if got := queue.compared[0]; got.Verdict != domain.ComparisonPrefersB || got.Evaluator.Kind != "" || got.A.Output.URI != "" || got.B.Output.URI == "" {
		t.Fatalf("a failed arm a = %+v", got)
	}

	failing := &queueFake{claim: comparisonClaim(nil)}
	w, _, _ = comparisonWorker(t, failing, func(domain.EvaluationPair) (domain.JudgeVerdict, error) {
		return domain.JudgeVerdict{Usage: domain.Usage{InputTokens: 9, CostMinorUnits: 1, Currency: "EUR"}}, domain.ErrInvalidModelOutput
	})
	secret := inputExample("a")
	secret.Hidden = true
	w.Examples = &examplesFake{examples: []domain.GoldenExample{secret}}
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(failing.compared) != 1 || failing.compared[0].Verdict != "" || !strings.HasPrefix(failing.compared[0].Reason, "judging failed") ||
		failing.compared[0].EvaluatorUsage.CostMinorUnits != 1 {
		t.Fatalf("judging failure recorded %+v", failing.compared)
	}
	if reason := failing.finished[0]; !strings.HasPrefix(reason, string(domain.EvaluationFailed)+"|a gate example failed") || strings.Contains(reason, "example a") {
		t.Fatalf("finished = %q", reason)
	}

	unpriced := &queueFake{claim: comparisonClaim(nil)}
	w, _, _ = comparisonWorker(t, unpriced, func(domain.EvaluationPair) (domain.JudgeVerdict, error) {
		return domain.JudgeVerdict{Preferred: domain.RoleBaseline, Usage: domain.Usage{InputTokens: 9}, Unpriced: true}, nil
	})
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unpriced.finished[0], "its usage was not settled") {
		t.Fatalf("unpriced judge spend finished %v", unpriced.finished)
	}
}

func TestAgentEvaluationWorkerComparesWithARecordedBaseline(t *testing.T) {
	queue := &queueFake{claim: comparisonClaim(nil)}
	queue.claim.Run.Baseline = &domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, AgentID: "writer-a", BaselineID: 5}
	recorded := map[string]domain.ObjectRef{}
	for _, id := range []string{"a", "b", "c"} {
		recorded[id] = domain.ObjectRef{URI: "object://baselines/" + id, Digest: sha256Hex([]byte("human answer " + id))}
	}
	queue.claim.Baseline = &domain.EvaluationArm{Subject: *queue.claim.Run.Baseline, Outputs: recorded}
	w, outputs, pairs := comparisonWorker(t, queue, nil)
	inputsReader := w.Payloads
	w.Payloads = objectReaderFunc(func(ctx context.Context, ref domain.ObjectRef) ([]byte, error) {
		if id, found := strings.CutPrefix(ref.URI, "object://baselines/"); found {
			return []byte("human answer " + id), nil
		}
		return inputsReader.Read(ctx, ref)
	})
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.compared) != 3 || len(outputs.Objects) != 3 {
		t.Fatalf("compared %d, stored %d answers; a baseline answer is never stored again", len(queue.compared), len(outputs.Objects))
	}
	for _, pair := range *pairs {
		if !strings.HasPrefix(string(pair.Baseline.Output), "human answer") || pair.Baseline.Usage.CostMinorUnits != 0 {
			t.Fatalf("baseline arm = %+v", pair.Baseline)
		}
	}
	if got := queue.compared[0]; got.A.Output != recorded[got.ExampleID] {
		t.Fatalf("baseline output ref = %+v", got.A.Output)
	}

	tampered := &queueFake{claim: queue.claim}
	tampered.compared = nil
	w, _, _ = comparisonWorker(t, tampered, nil)
	w.Payloads = objectReaderFunc(func(ctx context.Context, ref domain.ObjectRef) ([]byte, error) {
		if strings.HasPrefix(ref.URI, "object://baselines/") {
			return []byte("rewritten"), nil
		}
		return inputsReader.Read(ctx, ref)
	})
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(tampered.compared) != 0 || !strings.HasPrefix(tampered.finished[0], string(domain.EvaluationFailed)+"|") {
		t.Fatalf("a baseline output not matching its digest compared %d, finished %v", len(tampered.compared), tampered.finished)
	}
}

func TestAgentEvaluationWorkerRefusesAnUncomposedComparison(t *testing.T) {
	for name, mutate := range map[string]func(*AgentEvaluationWorker, *queueFake){
		"no pair judge":  func(w *AgentEvaluationWorker, _ *queueFake) { w.PairJudge = nil },
		"no requester":   func(_ *AgentEvaluationWorker, q *queueFake) { q.claim.Run.RequestedBy = domain.PrincipalRef{} },
		"arm a unpinned": func(_ *AgentEvaluationWorker, q *queueFake) { q.claim.Baseline = nil },
		"no releases":    func(w *AgentEvaluationWorker, _ *queueFake) { w.Releases = nil },
	} {
		queue := &queueFake{claim: comparisonClaim(nil)}
		w, _, pairs := comparisonWorker(t, queue, nil)
		mutate(w, queue)
		if err := w.Execute(context.Background(), testLease); err != nil {
			t.Fatal(err)
		}
		if len(*pairs) != 0 || len(queue.compared) != 0 || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationFailed)+"|") {
			t.Fatalf("%s: judged %d, finished %v", name, len(*pairs), queue.finished)
		}
	}
	unstored := &queueFake{claim: comparisonClaim(nil)}
	w, _, _ := comparisonWorker(t, unstored, nil)
	w.Outputs = nil
	if err := w.Execute(context.Background(), testLease); err == nil || len(unstored.finished) != 0 {
		t.Fatalf("a worker without output storage is not composed: %v, %v", err, unstored.finished)
	}
	queue := &queueFake{claim: comparisonClaim(nil), recordErr: errors.New("store down")}
	w, _, _ = comparisonWorker(t, queue, nil)
	if err := w.Execute(context.Background(), testLease); err == nil || len(queue.finished) != 0 {
		t.Fatalf("a store failure must leave the run for reclaim: %v, %v", err, queue.finished)
	}
}
