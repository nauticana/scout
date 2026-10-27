package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestAgentEvaluationStoreRecordsAndReadsATrajectoryReference(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{qAgentEvalRenew: {{"support", int64(3)}}})
	store := &AgentEvaluationStore{DB: dbFake{query: query}}
	lease := domain.AgentEvaluationLease{TenantID: 7, RunID: 11, Token: 99, TTL: 90 * time.Second}
	ref := domain.ObjectRef{URI: "object://outputs/evaluation/trajectory/7/11/x", Digest: sha256Hex([]byte("x"))}
	result := domain.EvaluationResult{ExampleID: "refund", Role: domain.RoleCandidate, Trajectory: ref}
	if err := store.Record(ctx, lease, result); err != nil {
		t.Fatal(err)
	}
	if args := query.args[qAgentEvalPutResult]; len(args) != 18 || args[16] != ref.URI || args[17] != ref.Digest {
		t.Fatalf("result args = %v", args)
	}
	for _, bad := range []domain.ObjectRef{{URI: "object://x"}, {Digest: ref.Digest}, {URI: "object://x", Digest: "short"}, {URI: strings.Repeat("u", 501), Digest: ref.Digest}} {
		result.Trajectory = bad
		if err := store.Record(ctx, lease, result); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("trajectory %+v = %v", bad, err)
		}
	}

	query.rows[qAgentEvalTrajectory] = [][]any{{ref.URI, ref.Digest + " "}}
	got, err := store.Trajectory(ctx, domain.GoldenScopeDev, 7, 11, "refund", domain.RoleBaseline)
	if err != nil || got != ref {
		t.Fatalf("trajectory = %+v, %v", got, err)
	}
	if args := query.args[qAgentEvalTrajectory]; args[0] != int64(7) || args[1] != int64(11) || args[2] != "refund" || args[3] != "B" || args[4] != "dev" || args[5] != "dev" {
		t.Fatalf("trajectory args = %v", args)
	}
	query.rows[qAgentEvalTrajectory] = [][]any{{nil, nil}}
	if _, err := store.Trajectory(ctx, domain.GoldenScopeDev, 7, 11, "refund", domain.RoleCandidate); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a result without a trajectory = %v", err)
	}
	if _, err := store.Trajectory(ctx, domain.GoldenScopeDev, 7, 11, "refund", "X"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an unknown arm = %v", err)
	}
	if sql := agentEvaluationQueries[qAgentEvalTrajectory]; !strings.Contains(sql, "e.scope_code = ? OR ? = 'gate'") {
		t.Fatalf("the trajectory read must filter gate examples in the predicate: %s", sql)
	}
}

func TestAgentEvaluationStoreRecordsComparisonArmTrajectories(t *testing.T) {
	ctx := context.Background()
	query := newQueryFake(map[string][][]any{qAgentEvalRenew: {{"support", int64(3)}}, qAgentEvalPutComparison: {{"refund"}}, qAgentEvalAddTotals: {{int64(11)}}})
	store := &AgentEvaluationStore{DB: dbFake{query: query}}
	lease := domain.AgentEvaluationLease{TenantID: 7, RunID: 11, Token: 99, TTL: 90 * time.Second}
	ref := domain.ObjectRef{URI: "object://outputs/t", Digest: sha256Hex([]byte("t"))}
	comparison := domain.CaseComparison{ExampleID: "refund", Verdict: domain.ComparisonTie,
		A: domain.EvaluationResult{Trajectory: ref}, B: domain.EvaluationResult{}}
	if err := store.RecordComparison(ctx, lease, comparison); err != nil {
		t.Fatal(err)
	}
	arms := query.history[qAgentEvalPutArm]
	if len(arms) != 2 || arms[0][14] != ref.URI || arms[0][15] != ref.Digest || arms[1][14] != nil || arms[1][15] != nil {
		t.Fatalf("arm args = %v", arms)
	}
	comparison.B.Trajectory = domain.ObjectRef{URI: "object://half"}
	if err := store.RecordComparison(ctx, lease, comparison); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a half trajectory reference = %v", err)
	}
}

func TestStudioEvaluationReadsATrajectoryOnlyWhenItMayAndItVerifies(t *testing.T) {
	ctx := context.Background()
	content, err := encodeTrajectory([]domain.TrajectoryEvent{{Sequence: 1, Kind: domain.TrajectoryState, Name: ReplayCompleted, Payload: []byte(`"done"`)}})
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.ObjectRef{URI: "object://outputs/t", Digest: sha256Hex(content)}
	reader := func(stored []byte) objectReaderFunc {
		return func(context.Context, domain.ObjectRef) ([]byte, error) { return stored, nil }
	}
	service, _, runs, _ := studioEvaluation(grants{domain.StudioView: true})
	runs.trajectory, service.Outputs = ref, reader(content)
	trajectory, err := service.Trajectory(ctx, studioActor, 11, "refund", domain.RoleCandidate)
	if err != nil || len(trajectory.Events) != 1 || trajectory.Events[0].Name != ReplayCompleted || trajectory.ExampleID != "refund" {
		t.Fatalf("trajectory = %+v, %v", trajectory, err)
	}
	for _, scope := range runs.scopes {
		if scope != domain.GoldenScopeDev {
			t.Fatalf("a Studio actor read in the %s scope", scope)
		}
	}
	if _, err := service.Trajectory(ctx, studioActor, 11, "refund", domain.RoleBaseline); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("arm a of a single-subject run = %v", err)
	}

	tampered, _, tamperedRuns, _ := studioEvaluation(grants{domain.StudioView: true})
	tamperedRuns.trajectory, tampered.Outputs = ref, reader([]byte(`{"events":[]}`))
	if _, err := tampered.Trajectory(ctx, studioActor, 11, "refund", domain.RoleCandidate); err == nil || errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a digest mismatch must be a server fault: %v", err)
	}

	bounded, _, boundedRuns, _ := studioEvaluation(grants{domain.StudioView: true})
	boundedRuns.trajectory, bounded.Outputs, bounded.MaxTrajectoryBytes = ref, reader(content), len(content)-1
	if _, err := bounded.Trajectory(ctx, studioActor, 11, "refund", domain.RoleCandidate); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an oversized trajectory = %v", err)
	}

	stranger, _, strangerRuns, _ := studioEvaluation(grants{})
	strangerRuns.trajectory, stranger.Outputs = ref, reader(content)
	if _, err := stranger.Trajectory(ctx, studioActor, 11, "refund", domain.RoleCandidate); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a run the actor cannot VIEW = %v", err)
	}
	if len(strangerRuns.roles) != 0 {
		t.Fatal("an unauthorized actor reached the trajectory reference")
	}

	missing, _, _, _ := studioEvaluation(grants{domain.StudioView: true})
	missing.Outputs = reader(content)
	if _, err := missing.Trajectory(ctx, studioActor, 11, "refund", domain.RoleCandidate); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a result without a trajectory = %v", err)
	}
	unread, _, _, _ := studioEvaluation(grants{domain.StudioView: true})
	if _, err := unread.Trajectory(ctx, studioActor, 11, "refund", domain.RoleCandidate); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no output reader = %v", err)
	}
}
