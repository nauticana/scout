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

type draftReplaysFake struct {
	mu       sync.Mutex
	examples []string
	settings int
}

func (replays *draftReplaysFake) ExecuteDraft(_ context.Context, tenantID int64, draft domain.AgentDefinition, settings []domain.EffectiveResource, example domain.GoldenExample) (domain.EvaluationCase, error) {
	replays.mu.Lock()
	defer replays.mu.Unlock()
	if tenantID != 7 || draft.DefinitionDigest != draftDigest {
		return domain.EvaluationCase{}, domain.ErrForbidden
	}
	replays.examples, replays.settings = append(replays.examples, example.ExampleID), len(settings)
	return domain.EvaluationCase{Output: []byte("replayed"), Usage: domain.Usage{CostMinorUnits: 2, Currency: "EUR"}, Trajectory: []domain.TrajectoryEvent{
		{Sequence: 1, Kind: domain.TrajectoryToolCall, Name: "orders.get", Payload: []byte(`{"id":1}`)},
		{Sequence: 2, Kind: domain.TrajectoryState, Name: ReplayCompleted, Payload: []byte("replayed")},
	}}, nil
}

// scriptedDraftWorker runs the draft claim with example b scripted.
func scriptedDraftWorker(t *testing.T, queue *queueFake) (*AgentEvaluationWorker, *[]domain.AgentTestRequest, *draftReplaysFake) {
	t.Helper()
	w, requests := draftWorker(t, queue, 5, "EUR")
	examples := w.Examples.(*examplesFake)
	examples.examples[1].Script = domain.ObjectRef{URI: "object://cases/script-b", Digest: sha256Hex([]byte("script"))}
	queue.claim.Draft.ToolLoop = &domain.ToolLoopConfig{}
	replays := &draftReplaysFake{}
	w.DraftReplays = replays
	return w, requests, replays
}

func TestAgentEvaluationWorkerReplaysScriptedDraftExamplesAndStoresTheirTrajectories(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, requests, replays := scriptedDraftWorker(t, queue)
	w.Trajectory = &TrajectoryScorer{Revision: "t1"}
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 2 || len(replays.examples) != 1 || replays.examples[0] != "b" || replays.settings != 1 || queue.finished[0] != string(domain.EvaluationCompleted)+"|" {
		t.Fatalf("draft tests %d, replays %v, finished %v", len(*requests), replays.examples, queue.finished)
	}
	outputs := w.Outputs.(*fake.ObjectStorage)
	for _, result := range queue.recorded {
		trajectoryScores := 0
		for _, score := range result.Scores {
			if score.Evaluator.Kind == evaluatorKindTrajectory {
				trajectoryScores++
			}
		}
		if result.ExampleID != "b" {
			if result.Trajectory.URI != "" || trajectoryScores != 0 {
				t.Fatalf("a case without a trajectory stored or scored one: %+v", result)
			}
			continue
		}
		key := strings.TrimPrefix(result.Trajectory.URI, "object://outputs/")
		stored := outputs.Objects[key]
		if !strings.HasPrefix(key, "evaluation/trajectory/7/11/") || sha256Hex(stored) != result.Trajectory.Digest || trajectoryScores == 0 {
			t.Fatalf("trajectory %+v with %d trajectory scores, stored %s", result.Trajectory, trajectoryScores, stored)
		}
		events, err := decodeTrajectory(stored)
		if err != nil || len(events) != 2 || events[0].Name != "orders.get" {
			t.Fatalf("stored trajectory = %+v, %v", events, err)
		}
	}
}

func TestAgentEvaluationWorkerFailsAScriptedDraftRunItCannotReplay(t *testing.T) {
	for name, mutate := range map[string]func(*AgentEvaluationWorker, *queueFake){
		"no draft replay": func(w *AgentEvaluationWorker, _ *queueFake) { w.DraftReplays = nil },
		"no tool loop":    func(_ *AgentEvaluationWorker, q *queueFake) { q.claim.Draft.ToolLoop = nil },
	} {
		queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
		w, requests, replays := scriptedDraftWorker(t, queue)
		mutate(w, queue)
		if err := w.Execute(context.Background(), testLease); err != nil {
			t.Fatal(err)
		}
		if len(*requests) != 0 || len(replays.examples) != 0 || len(queue.recorded) != 0 || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationFailed)+"|") {
			t.Fatalf("%s: ran %d tests and %d replays before failing: %v", name, len(*requests), len(replays.examples), queue.finished)
		}
	}
}

func TestAgentEvaluationWorkerSettlesACaseWhoseTrajectoryCannotBeStored(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, _, _ := scriptedDraftWorker(t, queue)
	w.Outputs = &fake.ObjectStorage{Name: "outputs", PutFunc: func(context.Context, string, []byte) error { return errors.New("bucket down") }}
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	var stored *domain.EvaluationResult
	for i := range queue.recorded {
		if queue.recorded[i].ExampleID == "b" {
			stored = &queue.recorded[i]
		}
	}
	if stored == nil || stored.Usage.CostMinorUnits != 2 || stored.Scores != nil || !strings.HasPrefix(stored.Reason, "storing trajectory failed") ||
		!strings.HasPrefix(queue.finished[0], string(domain.EvaluationFailed)+"|example b failed") {
		t.Fatalf("recorded %+v, finished %v", stored, queue.finished)
	}
}

func TestAgentEvaluationWorkerRefusesAnOversizedTrajectoryBeforeStorage(t *testing.T) {
	queue := &queueFake{claim: draftClaim(map[string]domain.Usage{})}
	w, _, _ := scriptedDraftWorker(t, queue)
	w.MaxTrajectoryBytes = 1
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	var stored *domain.EvaluationResult
	for i := range queue.recorded {
		if queue.recorded[i].ExampleID == "b" {
			stored = &queue.recorded[i]
		}
	}
	if stored == nil || stored.Usage.CostMinorUnits != 2 || !strings.HasPrefix(stored.Reason, "storing trajectory failed: validation") ||
		len(w.Outputs.(*fake.ObjectStorage).Objects) != 0 {
		t.Fatalf("recorded %+v, objects %v", stored, w.Outputs.(*fake.ObjectStorage).Objects)
	}
}
