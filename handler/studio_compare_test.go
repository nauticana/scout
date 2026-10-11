package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type compareBackendFake struct {
	request domain.AgentCompareRequest
	record  domain.BaselineRecord
	runID   int64
	limit   int
	version int64
}

func (f *compareBackendFake) Compare(_ context.Context, _ domain.StudioActor, request domain.AgentCompareRequest) (domain.AgentEvaluationRun, error) {
	f.request = request
	return domain.AgentEvaluationRun{RunID: 12, AgentID: request.AgentID, Status: domain.EvaluationQueued,
		Subject:  domain.AgentEvaluationSubject{Kind: domain.SubjectDraft, DefinitionDigest: "d", Model: &domain.ModelReference{ProviderID: "p", ModelID: "m"}},
		Baseline: &domain.AgentEvaluationSubject{Kind: domain.SubjectBaseline, BaselineID: 4}}, nil
}

func (f *compareBackendFake) Comparison(_ context.Context, _ domain.StudioActor, runID int64, _ string, limit int) (domain.AgentComparisonPage, error) {
	f.runID, f.limit = runID, limit
	return domain.AgentComparisonPage{
		Run:   domain.AgentEvaluationRun{RunID: runID, Subject: domain.AgentEvaluationSubject{Kind: domain.SubjectDraft}, Baseline: &domain.AgentEvaluationSubject{Kind: domain.SubjectRelease, Version: "2"}},
		Tally: domain.ComparisonTally{A: 1, B: 2, Ties: 3, Failed: 1},
		Diff: &domain.ComparisonDiff{Release: "2", Models: []domain.ModelChange{{Modality: "text", Before: &domain.ModelReference{ModelID: "m1"}, After: &domain.ModelReference{ModelID: "m2"}}},
			Settings: []domain.SettingChange{{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Change: domain.DiffChanged, Before: json.RawMessage(`1`), After: json.RawMessage(`2`)}}},
		Cases: []domain.AgentCaseComparison{
			{ExampleID: "refund", Verdict: domain.ComparisonPrefersA, Confidence: 0.75, Evaluator: domain.EvaluatorVersion{Kind: "judge", Version: "j"},
				EvaluatorUsage: domain.Usage{CostMinorUnits: 3, Currency: "EUR"}, Exponent: 2,
				A: domain.AgentCaseResult{Answer: []byte("human"), Output: domain.ObjectRef{Digest: "da"}, Latency: time.Second},
				B: domain.AgentCaseResult{Answer: []byte("agent"), Usage: domain.Usage{CostMinorUnits: 5, Currency: "EUR"}, Exponent: 2}},
			{ExampleID: "greet", Reason: "judging failed: invalid model output"},
		},
		NextBefore: "greet",
	}, nil
}

func (f *compareBackendFake) RecordBaseline(_ context.Context, _ domain.StudioActor, record domain.BaselineRecord) (domain.EvaluationBaseline, error) {
	f.record = record
	return domain.EvaluationBaseline{BaselineID: 4, Name: record.Name, SetVersion: record.SetVersion, ExampleCount: len(record.Outputs)}, nil
}

func (f *compareBackendFake) Baselines(_ context.Context, _ domain.StudioActor, _ string, setVersion int64) ([]domain.EvaluationBaseline, error) {
	f.version = setVersion
	return []domain.EvaluationBaseline{{BaselineID: 4, SetVersion: setVersion}}, nil
}

func TestStudioCompareRoutes(t *testing.T) {
	h := &StudioCompareHandler{}
	routes := h.Routes()
	for _, path := range []string{api.StudioComparePath, api.StudioBaselinesPath, api.StudioBaselinePath} {
		if routes[path] == nil {
			t.Fatalf("missing route %s", path)
		}
	}
	if feature := h.Feature(); feature.ID != StudioFeatureCompare || feature.Version != 1 || len(feature.Actions) != 3 || len(routes) != 3 {
		t.Fatalf("feature = %+v, routes = %d", feature, len(routes))
	}
}

func TestStudioCompareHandlerMapsRequestsAndPages(t *testing.T) {
	backend := &compareBackendFake{}
	h := &StudioCompareHandler{Backend: backend}
	view, err := h.compare(context.Background(), domain.StudioActor{}, api.AgentCompareRequest{AgentName: "writer-a", SetVersion: 3,
		A: api.CompareSubject{BaselineID: 4}, B: api.CompareSubject{Draft: true, Model: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	if request := backend.request; request.SetVersion != 3 || request.A.BaselineID != 4 || !request.B.Draft || request.B.TextModel != "m" {
		t.Fatalf("request = %+v", request)
	}
	run := view.(api.AgentComparisonRun)
	if run.RunID != 12 || run.A.Subject != "baseline" || run.A.BaselineID != 4 || run.B.Subject != "draft" || run.B.Model != "m" || run.B.Provider != "p" {
		t.Fatalf("run = %+v", run)
	}

	view, err = h.comparison(context.Background(), domain.StudioActor{}, url.Values{"run_id": {"12"}, "limit": {"5"}})
	if err != nil {
		t.Fatal(err)
	}
	page := view.(api.AgentComparisonPage)
	if backend.runID != 12 || backend.limit != 5 || page.Tally != (api.ComparisonTally{A: 1, B: 2, Tie: 3, Failed: 1}) || page.NextBefore != "greet" {
		t.Fatalf("page = %+v", page)
	}
	decided, failed := page.Cases[0], page.Cases[1]
	if decided.Verdict != string(domain.ComparisonPrefersA) || decided.Evaluator == nil || decided.A.Output != "human" || decided.A.OutputDigest != "da" || decided.A.LatencyMs != 1000 ||
		decided.B.Cost.CostMinorUnits != 5 || decided.EvaluatorCost.CostMinorUnits != 3 || decided.EvaluatorCost.Exponent != 2 {
		t.Fatalf("decided = %+v", decided)
	}
	if failed.Verdict != "" || failed.Evaluator != nil || failed.Reason == "" || failed.A.Scores == nil {
		t.Fatalf("failed = %+v", failed)
	}
	if diff := page.Diff; diff == nil || diff.Release != "2" || diff.Models[0].Before != "m1" || diff.Models[0].After != "m2" ||
		len(diff.Settings) != 1 || diff.PromptSections == nil {
		t.Fatalf("diff = %+v", page.Diff)
	}
	_, err = h.comparison(context.Background(), domain.StudioActor{}, url.Values{"run_id": {"x"}})
	var apiErr *keelhandler.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("malformed run_id = %v", err)
	}

	view, err = h.recordBaseline(context.Background(), domain.StudioActor{}, api.BaselineRecordRequest{AgentName: "writer-a", SetVersion: 3, Name: "Desk",
		Outputs: []api.BaselineOutput{{CaseID: "refund", Output: "we refund"}}})
	if err != nil || view.(api.EvaluationBaseline).BaselineID != 4 || string(backend.record.Outputs[0].Output) != "we refund" || backend.record.Name != "Desk" {
		t.Fatalf("baseline = %+v, %v (record %+v)", view, err, backend.record)
	}
	view, err = h.baselines(context.Background(), domain.StudioActor{}, url.Values{"agent_name": {"writer-a"}, "set_version": {"3"}})
	if err != nil || len(view.([]api.EvaluationBaseline)) != 1 || backend.version != 3 {
		t.Fatalf("baselines = %+v, %v", view, err)
	}
	if _, err = h.baselines(context.Background(), domain.StudioActor{}, url.Values{"set_version": {"three"}}); !errors.As(err, &apiErr) {
		t.Fatalf("malformed set_version = %v", err)
	}
}
