package evaluation

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/dataplane"
)

// Studio queues a release arm without one of its skills, and only a release arm.
func TestStudioCompareQueuesAReleaseWithoutOneSkill(t *testing.T) {
	ctx := context.Background()
	service, _, runs, _ := studioEvaluation(grants{domain.StudioTest: true})
	if _, err := service.Compare(ctx, studioActor, domain.AgentCompareRequest{AgentID: "writer-a", SetVersion: 3,
		A: domain.CompareSubject{Release: "v2"}, B: domain.CompareSubject{Release: "v2", OmitSkill: " audit "}}); err != nil {
		t.Fatal(err)
	}
	if subject := runs.enqueued.Subject; subject.Kind != domain.SubjectRelease || subject.Version != "v2" || subject.OmittedSkill != "audit" ||
		runs.enqueued.Baseline.Subject.OmittedSkill != "" {
		t.Fatalf("enqueued = %+v", runs.enqueued)
	}
	for name, request := range map[string]domain.AgentCompareRequest{
		"draft":   {A: domain.CompareSubject{Release: "v2"}, B: domain.CompareSubject{Draft: true, OmitSkill: "audit"}},
		"control": {A: domain.CompareSubject{Release: "v2"}, B: domain.CompareSubject{Control: true, OmitSkill: "audit"}},
	} {
		request.AgentID, request.SetVersion = "writer-a", 3
		if _, err := service.Compare(ctx, studioActor, request); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
}

func TestAgentEvaluationStoreRefusesASkillOmittedFromAnythingButARelease(t *testing.T) {
	release := domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v1", OmittedSkill: "audit"}
	ablated := domain.AgentEvaluationEnqueue{TenantID: 7, AgentID: "writer-a", SetVersion: 3, Scope: domain.GoldenScopeDev, Subject: release}
	if err := validEnqueue(ablated); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a single-arm run omitting a skill would count toward the gate: %v", err)
	}
	ablated.Baseline = &domain.EvaluationArm{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v1"}}
	if err := validEnqueue(ablated); err != nil {
		t.Fatal(err)
	}
	control := ablated
	control.Subject = domain.AgentEvaluationSubject{Kind: domain.SubjectControl, AgentID: "writer-a", Version: "v1"}
	control.Baseline = &domain.EvaluationArm{Subject: release}
	if err := validEnqueue(control); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a control of an ablated release = %v", err)
	}
	baseline := ablated
	baseline.Subject = domain.AgentEvaluationSubject{Kind: domain.SubjectControl, AgentID: "writer-a", Version: "v1", OmittedSkill: "audit"}
	baseline.Baseline = &domain.EvaluationArm{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v1"}}
	if err := validEnqueue(baseline); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a control omitting a skill = %v", err)
	}
}

func TestAgentEvaluationStoreReadsTheOmittedSkillOfEachArm(t *testing.T) {
	row := comparisonRunRow(nil)
	row[18], row[19], row[25], row[26] = string(domain.SubjectRelease), "v1", string(domain.SubjectRelease), "v1"
	row[27], row[28], row[31] = sha256Hex([]byte("v1")), nil, "audit"
	store := &AgentEvaluationStore{DB: dbFake{query: newQueryFake(map[string][][]any{qAgentEvalRun: {row}})}}
	run, err := store.Run(context.Background(), domain.GoldenScopeDev, 7, 11)
	if err != nil || run.Subject.OmittedSkill != "audit" || run.Baseline == nil || run.Baseline.OmittedSkill != "" {
		t.Fatalf("run = %+v, %v", run, err)
	}
}

// The worker replays an ablated arm through the release executor with the skill withheld.
func TestAgentEvaluationWorkerReplaysTheArmWithoutItsSkill(t *testing.T) {
	claim := comparisonClaim(nil)
	claim.Run.Subject = domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v1", DefinitionDigest: claim.Run.Baseline.DefinitionDigest, OmittedSkill: "audit"}
	claim.Draft, claim.DraftSettings = nil, nil
	queue := &queueFake{claim: claim}
	w, _, _ := comparisonWorker(t, queue, nil)
	var omitted []string
	w.Releases = fake.CaseExecutorFunc(func(_ context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, _ []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		omitted = append(omitted, subject.OmitSkill)
		return domain.EvaluationCase{Output: []byte(subject.OmitSkill + example.ExampleID), Usage: domain.Usage{CostMinorUnits: 2, Currency: "EUR"}}, nil
	})
	w.Concurrency = 1
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	withheld := 0
	for _, skill := range omitted {
		if skill == "audit" {
			withheld++
		}
	}
	if len(omitted) != 2*len(queue.compared) || withheld != len(queue.compared) {
		t.Fatalf("omitted = %v over %d comparisons", omitted, len(queue.compared))
	}
}

func TestWithheldSkillSandboxRefusesOnlyTheWithheldSkill(t *testing.T) {
	inner := &RecordingSandbox{Responses: map[string]domain.ToolResult{domain.UseSkillToolID: {Output: []byte(`{"skill":"x"}`)}, "crawl": {Output: []byte(`{}`)}}}
	sandbox := withheldSkillSandbox{ToolSandbox: inner, skillID: "audit"}
	call := func(toolID, args string) error {
		_, err := sandbox.Invoke(context.Background(), domain.ToolCall{TenantContext: domain.TenantContext{TenantID: 7}, ToolID: toolID, Arguments: []byte(args)})
		return err
	}
	if err := call(domain.UseSkillToolID, `{"skill":"audit"}`); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("withheld skill = %v", err)
	}
	if err := call(domain.UseSkillToolID, `{"skill":"rewrite"}`); err != nil {
		t.Fatalf("another skill = %v", err)
	}
	if err := call("crawl", `{"skill":"audit"}`); err != nil {
		t.Fatalf("another tool = %v", err)
	}
	if len(inner.Recorded()) != 3 {
		t.Fatalf("recorded = %d, want every call", len(inner.Recorded()))
	}
}

// A release replayed without a skill refuses use_skill for it, recorded as a failed call.
func TestReleaseCaseExecutorWithholdsTheOmittedSkill(t *testing.T) {
	loadCall := func() domain.ModelResult {
		return domain.ModelResult{ToolCalls: []domain.ModelToolCall{{CallID: nextCallID(), Name: domain.UseSkillToolID, Arguments: []byte(`{"skill":"audit"}`)}},
			FinishReason: domain.FinishReasonToolCalls}
	}
	replay := func(omit string) domain.EvaluationCase {
		harness := newReplayHarness(t, loadCall(), domain.ModelResult{Output: []byte("done")})
		harness.executor.Requests = requestBuilderFunc(func(ctx context.Context, input domain.StepInput) (domain.ModelRequest, error) {
			dataplane.SkillWithholdingFrom(ctx).Drop(nil)
			return domain.ModelRequest{TenantContext: domain.TenantContext{TenantID: 7}, Prompt: input.Input, MaxOutputTokens: 1024}, nil
		})
		harness.sandbox.Responses[domain.UseSkillToolID] = domain.ToolResult{Output: []byte(`{"skill":"audit","procedure":"p","tools":[]}`)}
		harness.executor.Registry = &fake.ToolRegistry{ListFunc: func(context.Context, int64, string, string) ([]domain.ToolDefinition, error) {
			return []domain.ToolDefinition{{ToolID: domain.UseSkillToolID, Version: "2", InputSchema: []byte(`{"type":"object"}`), ReadOnly: true}}, nil
		}}
		subject := replaySubject()
		subject.OmitSkill = omit
		evalCase, err := harness.executor.Execute(context.Background(), subject, replayExample(), nil)
		if err != nil || len(harness.sandbox.Recorded()) != 1 {
			t.Fatalf("omit %q: %+v, %v", omit, evalCase, err)
		}
		return evalCase
	}
	if observation := replay("").Trajectory[1]; observation.Kind != domain.TrajectoryObservation || observation.Failed {
		t.Fatalf("a bound skill = %+v", observation)
	}
	if observation := replay("audit").Trajectory[1]; observation.Kind != domain.TrajectoryObservation || !observation.Failed {
		t.Fatalf("the withheld skill = %+v", observation)
	}
	unaware := newReplayHarness(t)
	subject := replaySubject()
	subject.OmitSkill = "audit"
	if _, err := unaware.executor.Execute(context.Background(), subject, replayExample(), nil); !errors.Is(err, domain.ErrCapabilityUnsupported) || len(unaware.requests) != 0 {
		t.Fatalf("a builder that leaves the skill in the index = %v after %d model calls", err, len(unaware.requests))
	}
}

func TestAgentEvaluationStorePinsTheOmittedSkillOnlyWhenTheReleaseBindsIt(t *testing.T) {
	ctx := context.Background()
	ablated := draftEnqueue()
	ablated.Subject, ablated.Draft, ablated.DraftSettings = domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v2"}, nil, nil
	ablated.Baseline = &domain.EvaluationArm{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v2", OmittedSkill: "audit"}}
	if _, err := (&AgentEvaluationStore{DB: dbFake{query: newQueryFake(enqueueRows())}}).Enqueue(ctx, ablated); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("an unbound skill = %v", err)
	}
	rows := enqueueRows()
	rows[qAgentEvalBoundSkill] = [][]any{{"audit"}}
	query := newQueryFake(rows)
	run, err := (&AgentEvaluationStore{DB: dbFake{query: query}}).Enqueue(ctx, ablated)
	if err != nil || run.Subject.OmittedSkill != "" || run.Baseline.OmittedSkill != "audit" {
		t.Fatalf("run = %+v, %v", run, err)
	}
	if args := query.args[qAgentEvalBoundSkill]; args[0] != int64(7) || args[1] != "writer-a" || args[2] != "v2" || args[3] != "audit" {
		t.Fatalf("bound skill args = %v", args)
	}
	if args := query.args[qAgentEvalInsertSubj]; args[len(args)-1] != "audit" {
		t.Fatalf("subject args = %v", args)
	}
}
