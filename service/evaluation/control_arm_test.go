package evaluation

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/workingset"
)

// replayWorkingSets compiles and records a set per turn and names the selected ids in the prompt.
func replayWorkingSets(offered ...domain.WorkingSetCandidate) *ReplayWorkingSets {
	return &ReplayWorkingSets{
		Source: workingset.Fixed{Offered: domain.WorkingSetCandidates{Candidates: offered}},
		Limits: domain.WorkingSetLimits{MaxTokens: 20},
		Requests: func(compiler contract.WorkingSetCompiler, records contract.WorkingSetRecorder) contract.ToolLoopRequestBuilder {
			return requestBuilderFunc(func(ctx context.Context, input domain.StepInput) (domain.ModelRequest, error) {
				set, err := compiler.Compile(ctx, domain.WorkingSetRequest{TenantContext: domain.TenantContext{TenantID: 7}, Principal: input.Principal, RequestID: input.RequestID, Task: string(input.Input)})
				if err != nil {
					return domain.ModelRequest{}, err
				}
				if err := records.RecordWorkingSet(ctx, input.Owner, set); err != nil {
					return domain.ModelRequest{}, err
				}
				var ids []string
				for _, candidate := range set.Selected {
					ids = append(ids, candidate.ID)
				}
				return domain.ModelRequest{TenantContext: domain.TenantContext{TenantID: 7}, Prompt: []byte(strings.Join(ids, ",")), MaxOutputTokens: 1024}, nil
			})
		},
	}
}

func controlCandidates() []domain.WorkingSetCandidate {
	filler := domain.WorkingSetCandidate{ID: "filler", Content: "noise", Provenance: "crm", Standing: domain.StandingCurrent, TokenCost: 15, AuthorityRank: 5}
	limit := domain.WorkingSetCandidate{ID: "limit", Key: "credit_limit", Content: "5000", Provenance: "crm", Standing: domain.StandingCurrent, TokenCost: 10, Required: true}
	return []domain.WorkingSetCandidate{filler, limit}
}

// A replay records its working set on the case; the control replays the reference's candidates
// truncated in offer order to the reference's tokens, dropping the required item.
func TestReleaseCaseExecutorRecordsTheWorkingSetAndReplaysItsControl(t *testing.T) {
	answer := domain.ModelResult{Output: []byte("done"), Usage: domain.Usage{InputTokens: 2, OutputTokens: 1}}
	harness := newReplayHarness(t, answer)
	harness.executor.WorkingSets = replayWorkingSets(controlCandidates()...)
	reference, err := harness.executor.Execute(context.Background(), replaySubject(), replayExample(), nil)
	if err != nil || reference.WorkingSet == nil || reference.WorkingSet.Assurance != domain.AssuranceVerified || string(harness.requests[0].Prompt) != "limit" ||
		len(reference.WorkingSet.Offered) != 2 || reference.WorkingSet.TokenCost != 10 {
		t.Fatalf("reference = %+v, %v", reference.WorkingSet, err)
	}
	control := newReplayHarness(t, answer)
	control.executor.WorkingSets = replayWorkingSets()
	subject := replaySubject()
	subject.Control = reference.WorkingSet
	evalCase, err := control.executor.Execute(context.Background(), subject, replayExample(), nil)
	if err != nil || evalCase.WorkingSet == nil || evalCase.WorkingSet.Assurance != domain.AssuranceUnverified ||
		string(control.requests[0].Prompt) != "" || !slices.Equal(evalCase.WorkingSet.Excluded, []string{"filler", "limit"}) {
		t.Fatalf("control = %+v, %v", evalCase.WorkingSet, err)
	}
	if scores := assuranceScores(t, domain.EvaluationCase{Example: domain.GoldenExample{Assurance: domain.AssuranceLabels{RequiredEvidence: []string{"limit"}}}, WorkingSet: evalCase.WorkingSet}); scores[domain.MetricRequiredEvidencePreservation].Value != 0 {
		t.Fatalf("the control dropped the required item: %+v", scores)
	}
	unaware := newReplayHarness(t, answer)
	if _, err := unaware.executor.Execute(context.Background(), subject, replayExample(), nil); !errors.Is(err, domain.ErrCapabilityUnsupported) {
		t.Fatalf("control without replay working sets = %v", err)
	}
	unaware.executor.WorkingSets = &ReplayWorkingSets{}
	if _, err := unaware.executor.Execute(context.Background(), replaySubject(), replayExample(), nil); err == nil {
		t.Fatal("replay working sets without a source and a builder must be refused")
	}
}

// The worker hands arm a's working set to the control arm and fails a control whose reference recorded none.
func TestAgentEvaluationWorkerRunsTheControlOverTheReferencesWorkingSet(t *testing.T) {
	claim := comparisonClaim(nil)
	claim.Run.Subject = domain.AgentEvaluationSubject{Kind: domain.SubjectControl, AgentID: "writer-a", Version: "v1", DefinitionDigest: claim.Run.Baseline.DefinitionDigest}
	claim.Draft, claim.DraftSettings = nil, nil
	queue := &queueFake{claim: claim}
	w, _, _ := comparisonWorker(t, queue, nil)
	w.Evaluators = []contract.HeuristicEvaluator{&AssuranceEvaluator{Revision: "1"}}
	set := domain.WorkingSet{Assurance: domain.AssuranceVerified, TokenCost: 10, Offered: controlCandidates(), Selected: controlCandidates()[1:], Excluded: []string{"filler"}}
	var controls []*domain.WorkingSet
	w.Releases = fake.CaseExecutorFunc(func(_ context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, _ []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		if subject.Control == nil {
			return domain.EvaluationCase{Output: []byte("reference"), WorkingSet: &set, Usage: domain.Usage{CostMinorUnits: 2, Currency: "EUR"}}, nil
		}
		controls = append(controls, subject.Control)
		return domain.EvaluationCase{Output: []byte("control"), WorkingSet: &domain.WorkingSet{Assurance: domain.AssuranceUnverified, Excluded: []string{"filler", "limit"}},
			Usage: domain.Usage{CostMinorUnits: 2, Currency: "EUR"}}, nil
	})
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(controls) != 2 || controls[0].TokenCost != 10 || len(queue.compared) != 2 {
		t.Fatalf("controls = %+v, compared = %d", controls, len(queue.compared))
	}
	for _, comparison := range queue.compared {
		if comparison.A.Scores[0].Metric != domain.MetricRequiredEvidencePreservation || comparison.A.Scores[0].Value != 1 ||
			comparison.B.Scores[0].Value != 0 || comparison.B.Scores[len(comparison.B.Scores)-1].Metric != domain.MetricCorrectness {
			t.Fatalf("scores a = %+v, b = %+v", comparison.A.Scores, comparison.B.Scores)
		}
	}

	w.Releases = fake.CaseExecutorFunc(func(context.Context, domain.EvaluationSubject, domain.GoldenExample, []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
		return domain.EvaluationCase{Output: []byte("no set"), Usage: domain.Usage{CostMinorUnits: 2, Currency: "EUR"}}, nil
	})
	queue = &queueFake{claim: claim}
	w.Queue = queue
	if err := w.Execute(context.Background(), testLease); err != nil {
		t.Fatal(err)
	}
	if len(queue.finished) != 1 || !strings.HasPrefix(queue.finished[0], string(domain.EvaluationFailed)) || !strings.Contains(queue.compared[0].Reason, "arm b failed") {
		t.Fatalf("finished = %v, compared = %+v", queue.finished, queue.compared)
	}
}

// Studio queues a control only as arm b of a release named by arm a.
func TestStudioCompareQueuesAControlOfTheReferenceRelease(t *testing.T) {
	ctx := context.Background()
	service, _, runs, _ := studioEvaluation(grants{domain.StudioTest: true})
	if _, err := service.Compare(ctx, studioActor, domain.AgentCompareRequest{AgentID: "writer-a", SetVersion: 3,
		A: domain.CompareSubject{Release: " v2 "}, B: domain.CompareSubject{Control: true}}); err != nil {
		t.Fatal(err)
	}
	if subject := runs.enqueued.Subject; subject.Kind != domain.SubjectControl || subject.Version != "v2" || runs.enqueued.Baseline.Subject.Version != "v2" {
		t.Fatalf("enqueued = %+v", runs.enqueued)
	}
	for name, request := range map[string]domain.AgentCompareRequest{
		"control as a":       {A: domain.CompareSubject{Control: true}, B: domain.CompareSubject{Release: "v2"}},
		"control of a draft": {A: domain.CompareSubject{Draft: true}, B: domain.CompareSubject{Control: true}},
		"control and draft":  {A: domain.CompareSubject{Release: "v2"}, B: domain.CompareSubject{Control: true, Draft: true}},
	} {
		request.AgentID, request.SetVersion = "writer-a", 3
		if _, err := service.Compare(ctx, studioActor, request); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v, want validation", name, err)
		}
	}
}

func TestAgentEvaluationStoreRefusesAControlWithoutItsReference(t *testing.T) {
	control := domain.AgentEvaluationEnqueue{TenantID: 7, AgentID: "writer-a", SetVersion: 3, Scope: domain.GoldenScopeDev,
		Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectControl, AgentID: "writer-a", Version: "v1"}}
	for name, baseline := range map[string]*domain.EvaluationArm{
		"alone":           nil,
		"another release": {Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v2"}},
		"a draft":         {Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectDraft, AgentID: "writer-a"}},
	} {
		control.Baseline = baseline
		if err := validEnqueue(control); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	control.Baseline = &domain.EvaluationArm{Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, AgentID: "writer-a", Version: "v1"}}
	if err := validEnqueue(control); err != nil {
		t.Fatal(err)
	}
}
