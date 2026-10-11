package handler

import (
	"context"
	"net/http"
	"net/url"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureCompare is the Studio feature StudioCompareHandler serves.
const StudioFeatureCompare = "compare"

// StudioCompareHandler compares two evaluation subjects of an agent and keeps the human
// baselines a comparison may name.
type StudioCompareHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioCompareBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioCompareHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureCompare, Version: 1, Actions: []string{
		string(domain.StudioView), string(domain.StudioEdit), string(domain.StudioTest),
	}}
}

// Routes returns POST compare, GET compare?run_id=&before=&limit=, GET baselines?agent_name=&set_version=,
// and POST baseline.
func (h *StudioCompareHandler) Routes() map[string]http.HandlerFunc {
	read, queue := query(&h.AbstractHandler, h.comparison), command(&h.AbstractHandler, h.compare)
	return map[string]http.HandlerFunc{
		api.StudioComparePath: func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				queue(w, r)
				return
			}
			read(w, r)
		},
		api.StudioBaselinesPath: query(&h.AbstractHandler, h.baselines),
		api.StudioBaselinePath:  command(&h.AbstractHandler, h.recordBaseline),
	}
}

func (h *StudioCompareHandler) compare(ctx context.Context, actor domain.StudioActor, request api.AgentCompareRequest) (any, error) {
	run, err := h.Backend.Compare(ctx, actor, domain.AgentCompareRequest{
		AgentID: request.AgentName, SetVersion: request.SetVersion, A: compareSubject(request.A), B: compareSubject(request.B),
	})
	if err != nil {
		return nil, err
	}
	return apiComparisonRun(run), nil
}

func compareSubject(subject api.CompareSubject) domain.CompareSubject {
	return domain.CompareSubject{Draft: subject.Draft, TextModel: subject.Model, Release: subject.Release, BaselineID: subject.BaselineID}
}

func (h *StudioCompareHandler) comparison(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	runID, runErr := optionalInt(query.Get("run_id"), 0)
	limit, limitErr := optionalInt(query.Get("limit"), 0)
	if runErr != nil || limitErr != nil {
		return nil, badRequest("run_id and limit must be integers")
	}
	page, err := h.Backend.Comparison(ctx, actor, runID, query.Get("before"), int(limit))
	if err != nil {
		return nil, err
	}
	result := api.AgentComparisonPage{
		Run:        apiComparisonRun(page.Run),
		Tally:      api.ComparisonTally{A: page.Tally.A, B: page.Tally.B, Tie: page.Tally.Ties, Failed: page.Tally.Failed},
		Cases:      make([]api.CaseComparison, 0, len(page.Cases)),
		NextBefore: page.NextBefore,
	}
	for _, item := range page.Cases {
		comparison := api.CaseComparison{
			CaseID: item.ExampleID, Verdict: apiVerdict(item.Verdict), Confidence: item.Confidence, Rationale: item.Rationale, Reason: item.Reason,
			EvaluatorCost: apiCost(item.EvaluatorUsage, item.Exponent), A: apiComparedAnswer(item.A), B: apiComparedAnswer(item.B),
		}
		if item.Evaluator.Kind != "" {
			comparison.Evaluator = &api.EvaluatorVersion{Kind: item.Evaluator.Kind, Version: item.Evaluator.Version}
		}
		result.Cases = append(result.Cases, comparison)
	}
	if diff := page.Diff; diff != nil {
		result.Diff = &api.ComparisonDiff{
			Release: diff.Release, Models: make([]api.ModelChange, 0, len(diff.Models)),
			PromptSections: make([]api.PromptSectionChange, 0, len(diff.PromptSections)), Settings: make([]api.SettingChange, 0, len(diff.Settings)),
		}
		for _, change := range diff.Models {
			result.Diff.Models = append(result.Diff.Models, api.ModelChange{Modality: change.Modality, Before: referenceID(change.Before), After: referenceID(change.After)})
		}
		for _, change := range diff.PromptSections {
			result.Diff.PromptSections = append(result.Diff.PromptSections, api.PromptSectionChange{LanguageCode: change.LanguageCode,
				PromptSectionID: change.PromptSectionID, Caption: change.Caption, Change: string(change.Change)})
		}
		for _, change := range diff.Settings {
			result.Diff.Settings = append(result.Diff.Settings, api.SettingChange{ResourceKind: string(change.ResourceKind),
				ResourceID: change.ResourceID, Change: string(change.Change), Before: change.Before, After: change.After})
		}
	}
	return result, nil
}

func apiVerdict(verdict domain.ComparisonVerdict) string {
	switch verdict {
	case domain.ComparisonPrefersA:
		return "a"
	case domain.ComparisonPrefersB:
		return "b"
	case domain.ComparisonTie:
		return "tie"
	}
	return ""
}

func apiComparedAnswer(arm domain.AgentCaseResult) api.ComparedAnswer {
	return api.ComparedAnswer{
		Output: string(arm.Answer), OutputOmitted: arm.AnswerOmitted, OutputDigest: arm.Output.Digest, Scores: apiScores(arm.Scores),
		LatencyMs: arm.Latency.Milliseconds(), Cost: apiCost(arm.Usage, arm.Exponent),
	}
}

func apiComparedSubject(subject domain.AgentEvaluationSubject) api.ComparedSubject {
	result := api.ComparedSubject{Subject: string(subject.Kind), Release: subject.Version, BaselineID: subject.BaselineID, DefinitionDigest: subject.DefinitionDigest}
	if subject.Model != nil {
		result.Provider, result.Model = subject.Model.ProviderID, subject.Model.ModelID
	}
	return result
}

func apiComparisonRun(run domain.AgentEvaluationRun) api.AgentComparisonRun {
	result := api.AgentComparisonRun{
		RunID: run.RunID, AgentName: run.AgentID, GoldenSetID: run.GoldenSetID, SetVersion: run.SetVersion, B: apiComparedSubject(run.Subject),
		Status: run.Status, QueuedAt: run.QueuedAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
		Samples: run.Samples, HiddenResults: run.HiddenResults, Cost: apiCost(run.Usage, run.Exponent), Reason: run.Reason,
	}
	if run.Baseline != nil {
		result.A = apiComparedSubject(*run.Baseline)
	}
	return result
}

func (h *StudioCompareHandler) baselines(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	setVersion, err := optionalInt(query.Get("set_version"), 0)
	if err != nil {
		return nil, badRequest("set_version must be an integer")
	}
	baselines, err := h.Backend.Baselines(ctx, actor, query.Get("agent_name"), setVersion)
	if err != nil {
		return nil, err
	}
	result := make([]api.EvaluationBaseline, 0, len(baselines))
	for _, baseline := range baselines {
		result = append(result, apiBaseline(baseline))
	}
	return result, nil
}

func (h *StudioCompareHandler) recordBaseline(ctx context.Context, actor domain.StudioActor, request api.BaselineRecordRequest) (any, error) {
	outputs := make([]domain.BaselineOutput, 0, len(request.Outputs))
	for _, output := range request.Outputs {
		outputs = append(outputs, domain.BaselineOutput{CaseID: output.CaseID, Output: []byte(output.Output)})
	}
	baseline, err := h.Backend.RecordBaseline(ctx, actor, domain.BaselineRecord{
		AgentID: request.AgentName, SetVersion: request.SetVersion, Name: request.Name, Outputs: outputs,
	})
	if err != nil {
		return nil, err
	}
	return apiBaseline(baseline), nil
}

func apiBaseline(baseline domain.EvaluationBaseline) api.EvaluationBaseline {
	return api.EvaluationBaseline{
		BaselineID: baseline.BaselineID, GoldenSetID: baseline.GoldenSetID, SetVersion: baseline.SetVersion, Name: baseline.Name,
		ExampleCount: baseline.ExampleCount, ContentDigest: baseline.ContentDigest, RecordedAt: baseline.RecordedAt,
	}
}
