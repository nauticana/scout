package handler

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type evaluationBackendFake struct {
	contract.StudioEvaluationBackend
	change  domain.GoldenCaseChange
	request domain.AgentEvaluationRequest
	runID   int64
	before  string
	limit   int
}

func (f *evaluationBackendFake) Cases(_ context.Context, _ domain.StudioActor, agentID string) (domain.GoldenCaseSet, error) {
	return domain.GoldenCaseSet{AgentID: agentID, GoldenSetID: "support", Revision: 4, HiddenCases: 1, Cases: []domain.GoldenCase{
		{CaseID: "refund", Name: "Refund", Input: []byte("hi"), ExpectedBehavior: []byte("explains")},
		{CaseID: "secret", Name: "Secret", Input: []byte("probe"), Hidden: true},
	}}, nil
}

func (f *evaluationBackendFake) ChangeCases(ctx context.Context, actor domain.StudioActor, change domain.GoldenCaseChange) (domain.GoldenCaseSet, error) {
	f.change = change
	return f.Cases(ctx, actor, change.AgentID)
}

func (f *evaluationBackendFake) Evaluate(_ context.Context, _ domain.StudioActor, request domain.AgentEvaluationRequest) (domain.AgentEvaluationRun, error) {
	f.request = request
	return domain.AgentEvaluationRun{RunID: 11, AgentID: request.AgentID, Status: domain.EvaluationQueued,
		Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectDraft, DefinitionDigest: "d"}}, nil
}

func (f *evaluationBackendFake) Evaluation(_ context.Context, _ domain.StudioActor, runID int64, before string, limit int) (domain.AgentEvaluationPage, error) {
	f.runID, f.before, f.limit = runID, before, limit
	return domain.AgentEvaluationPage{
		Run: domain.AgentEvaluationRun{RunID: runID, Usage: domain.Usage{CostMinorUnits: 1250, Currency: "JPY"}, Exponent: 0, HiddenResults: 2},
		Results: []domain.AgentCaseResult{{ExampleID: "refund", Latency: 1500 * time.Millisecond, Exponent: 2,
			Usage: domain.Usage{CostMinorUnits: 7, Currency: "EUR"}, EvaluatorUsage: domain.Usage{InputTokens: 40, CostMinorUnits: 3, Currency: "EUR"},
			Scores: []domain.EvaluationScore{{Metric: "correctness", Value: 0.5, Evaluator: domain.EvaluatorVersion{Kind: "judge", Version: "j2"}, Rationale: "partial"}}}},
		NextBefore: "refund",
	}, nil
}

func TestStudioEvaluationRoutes(t *testing.T) {
	h := &StudioEvaluationHandler{}
	routes := h.Routes()
	for _, path := range []string{api.StudioCasesPath, api.StudioCasesFreezePath, api.StudioEvaluatePath, api.StudioEvaluationPath, api.StudioEvaluationTrajectoryPath} {
		if routes[path] == nil {
			t.Fatalf("missing route %s", path)
		}
	}
	if len(routes) != 5 {
		t.Fatalf("routes = %v", routes)
	}
	if feature := h.Feature(); feature.ID != StudioFeatureEvaluation || len(feature.Actions) != 3 {
		t.Fatalf("feature = %+v", feature)
	}
}

func TestStudioEvaluationHandlerMapsCasesAndNeverReturnsHiddenOnes(t *testing.T) {
	backend := &evaluationBackendFake{}
	h := &StudioEvaluationHandler{Backend: backend}
	view, err := h.cases(context.Background(), domain.StudioActor{}, url.Values{"agent_name": {"writer-a"}})
	if err != nil {
		t.Fatal(err)
	}
	set := view.(api.GoldenCaseSet)
	if set.AgentName != "writer-a" || len(set.Cases) != 1 || set.Cases[0].CaseID != "refund" || set.Cases[0].ExpectedBehavior != "explains" || set.HiddenCases != 1 {
		t.Fatalf("cases = %+v", set)
	}
	_, err = h.changeCases(context.Background(), domain.StudioActor{}, api.GoldenCaseChange{
		AgentName: "writer-a", Op: "put", ExpectedRevision: 4,
		Case: api.GoldenCase{CaseID: "refund", Name: "Refund", Input: "hi", Rubric: "rubric://tone", RiskTier: "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	change := backend.change
	if change.Op != domain.GoldenCasePut || change.ExpectedRevision != 4 || string(change.Case.Input) != "hi" || change.Case.RubricRef != "rubric://tone" ||
		change.Case.RiskTier != "high" || change.Case.ExpectedBehavior != nil || change.Case.Hidden {
		t.Fatalf("change = %+v", change)
	}
}

func TestStudioEvaluationHandlerMapsRunsAndPages(t *testing.T) {
	backend := &evaluationBackendFake{}
	h := &StudioEvaluationHandler{Backend: backend}
	view, err := h.evaluate(context.Background(), domain.StudioActor{}, api.AgentEvaluateRequest{AgentName: "writer-a", SetVersion: 3, Subject: api.EvaluationSubject{Draft: true}})
	if err != nil {
		t.Fatal(err)
	}
	if run := view.(api.AgentEvaluationRun); run.RunID != 11 || run.Subject != string(domain.SubjectDraft) || run.DefinitionDigest != "d" || !backend.request.Draft || backend.request.SetVersion != 3 {
		t.Fatalf("run = %+v, request = %+v", run, backend.request)
	}
	view, err = h.evaluation(context.Background(), domain.StudioActor{}, url.Values{"run_id": {"11"}, "before": {"zeta"}, "limit": {"20"}})
	if err != nil {
		t.Fatal(err)
	}
	page := view.(api.AgentEvaluationPage)
	if backend.runID != 11 || backend.before != "zeta" || backend.limit != 20 || page.NextBefore != "refund" {
		t.Fatalf("page request %d/%q/%d, page %+v", backend.runID, backend.before, backend.limit, page)
	}
	if page.Run.Cost.CostMinorUnits != 1250 || page.Run.Cost.Currency != "JPY" || page.Run.Cost.Exponent != 0 || page.Run.HiddenResults != 2 {
		t.Fatalf("run cost = %+v", page.Run)
	}
	result := page.Results[0]
	if result.Cost.CostMinorUnits != 7 || result.EvaluatorCost.CostMinorUnits != 3 || result.EvaluatorCost.InputTokens != 40 ||
		result.EvaluatorCost.Currency != "EUR" || result.EvaluatorCost.Exponent != 2 {
		t.Fatalf("costs = %+v and %+v, want the subject's and the evaluators' apart", result.Cost, result.EvaluatorCost)
	}
	if result.CaseID != "refund" || result.LatencyMs != 1500 || result.Cost.Exponent != 2 || result.Scores[0].Evaluator.Version != "j2" || result.Scores[0].Rationale != "partial" {
		t.Fatalf("result = %+v", result)
	}
	_, err = h.evaluation(context.Background(), domain.StudioActor{}, url.Values{"run_id": {"x"}})
	var apiErr *keelhandler.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("malformed run_id = %v", err)
	}
}
