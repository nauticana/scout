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

// StudioFeatureEvaluation is the Studio feature StudioEvaluationHandler serves.
const StudioFeatureEvaluation = "evaluation"

// StudioEvaluationHandler serves an agent's saved test cases and its evaluations.
type StudioEvaluationHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioEvaluationBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioEvaluationHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureEvaluation, Version: 1, Actions: []string{
		string(domain.StudioView), string(domain.StudioEdit), string(domain.StudioTest),
	}}
}

// Routes returns GET cases?agent_name=, POST cases, POST cases/freeze, POST evaluate,
// and GET evaluation?run_id=&before=&limit=.
func (h *StudioEvaluationHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioCasesPath:       h.casesRoute(),
		api.StudioCasesFreezePath: command(&h.AbstractHandler, h.freeze),
		api.StudioEvaluatePath:    command(&h.AbstractHandler, h.evaluate),
		api.StudioEvaluationPath:  query(&h.AbstractHandler, h.evaluation),
	}
}

// casesRoute reads with GET and changes with POST on one path.
func (h *StudioEvaluationHandler) casesRoute() http.HandlerFunc {
	read, change := query(&h.AbstractHandler, h.cases), command(&h.AbstractHandler, h.changeCases)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			change(w, r)
			return
		}
		read(w, r)
	}
}

func (h *StudioEvaluationHandler) cases(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	return apiCaseSet(h.Backend.Cases(ctx, actor, query.Get("agent_name")))
}

func (h *StudioEvaluationHandler) changeCases(ctx context.Context, actor domain.StudioActor, request api.GoldenCaseChange) (any, error) {
	return apiCaseSet(h.Backend.ChangeCases(ctx, actor, domain.GoldenCaseChange{
		AgentID: request.AgentName, Op: domain.GoldenCaseOp(request.Op), GoldenSetID: request.GoldenSetID, SetName: request.Name,
		CaseID: request.CaseID, ExpectedRevision: request.ExpectedRevision,
		Case: domain.GoldenCase{
			CaseID: request.Case.CaseID, Name: request.Case.Name, Input: []byte(request.Case.Input), Language: request.Case.Language,
			ExpectedBehavior: optionalBytes(request.Case.ExpectedBehavior), RubricRef: request.Case.Rubric, RiskTier: request.Case.RiskTier,
		},
	}))
}

func (h *StudioEvaluationHandler) freeze(ctx context.Context, actor domain.StudioActor, request api.GoldenCaseFreeze) (any, error) {
	set, version, err := h.Backend.FreezeCases(ctx, actor, request.AgentName, request.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	return api.GoldenCaseFrozen{Version: apiSetVersion(version), Set: toCaseSet(set)}, nil
}

func (h *StudioEvaluationHandler) evaluate(ctx context.Context, actor domain.StudioActor, request api.AgentEvaluateRequest) (any, error) {
	run, err := h.Backend.Evaluate(ctx, actor, domain.AgentEvaluationRequest{
		AgentID: request.AgentName, SetVersion: request.SetVersion, Draft: request.Subject.Draft, Release: request.Subject.Release,
	})
	if err != nil {
		return nil, err
	}
	return apiEvaluationRun(run), nil
}

func (h *StudioEvaluationHandler) evaluation(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	runID, runErr := optionalInt(query.Get("run_id"), 0)
	limit, limitErr := optionalInt(query.Get("limit"), 0)
	if runErr != nil || limitErr != nil {
		return nil, badRequest("run_id and limit must be integers")
	}
	page, err := h.Backend.Evaluation(ctx, actor, runID, query.Get("before"), int(limit))
	if err != nil {
		return nil, err
	}
	result := api.AgentEvaluationPage{Run: apiEvaluationRun(page.Run), Results: make([]api.AgentCaseResult, 0, len(page.Results)), NextBefore: page.NextBefore}
	for _, item := range page.Results {
		result.Results = append(result.Results, api.AgentCaseResult{
			CaseID: item.ExampleID, Scores: apiScores(item.Scores), LatencyMs: item.Latency.Milliseconds(), Cost: apiCost(item.Usage, item.Exponent),
			EvaluatorCost: apiCost(item.EvaluatorUsage, item.Exponent), NeedsHumanReview: item.NeedsHumanReview, Reason: item.Reason,
		})
	}
	return result, nil
}

func apiScores(scores []domain.EvaluationScore) []api.EvaluationScore {
	result := make([]api.EvaluationScore, 0, len(scores))
	for _, score := range scores {
		result = append(result, api.EvaluationScore{
			Metric: score.Metric, Value: score.Value, Confidence: score.Confidence, Rationale: score.Rationale, Critical: score.Critical,
			Evaluator: api.EvaluatorVersion{Kind: score.Evaluator.Kind, Version: score.Evaluator.Version},
		})
	}
	return result
}

func optionalBytes(value string) []byte {
	if value == "" {
		return nil
	}
	return []byte(value)
}

func apiCaseSet(set domain.GoldenCaseSet, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return toCaseSet(set), nil
}

func toCaseSet(set domain.GoldenCaseSet) api.GoldenCaseSet {
	result := api.GoldenCaseSet{
		AgentName: set.AgentID, GoldenSetID: set.GoldenSetID, Revision: set.Revision, HiddenCases: set.HiddenCases,
		Cases: make([]api.GoldenCase, 0, len(set.Cases)), Versions: make([]api.GoldenSetVersion, 0, len(set.Versions)),
	}
	for _, goldenCase := range set.Cases {
		if goldenCase.Hidden {
			continue
		}
		result.Cases = append(result.Cases, api.GoldenCase{
			CaseID: goldenCase.CaseID, Name: goldenCase.Name, Input: string(goldenCase.Input), Language: goldenCase.Language,
			ExpectedBehavior: string(goldenCase.ExpectedBehavior), Rubric: goldenCase.RubricRef, RiskTier: goldenCase.RiskTier,
			ModifiedAt: goldenCase.ModifiedAt, ReviewItemID: goldenCase.ReviewItemID, InputWithheld: goldenCase.InputWithheld,
		})
	}
	for _, version := range set.Versions {
		result.Versions = append(result.Versions, apiSetVersion(version))
	}
	return result
}

func apiSetVersion(version domain.GoldenSetVersion) api.GoldenSetVersion {
	return api.GoldenSetVersion{SetVersion: version.SetVersion, DatasetRevision: version.DatasetRevision, ExampleCount: version.ExampleCount, FrozenAt: version.FrozenAt}
}

func apiCost(usage domain.Usage, exponent int) api.EvaluationCost {
	return api.EvaluationCost{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CostMinorUnits: usage.CostMinorUnits, Currency: usage.Currency, Exponent: exponent}
}

func apiEvaluationRun(run domain.AgentEvaluationRun) api.AgentEvaluationRun {
	return api.AgentEvaluationRun{
		RunID: run.RunID, AgentName: run.AgentID, GoldenSetID: run.GoldenSetID, SetVersion: run.SetVersion,
		Subject: string(run.Subject.Kind), Release: run.Subject.Version, DefinitionDigest: run.Subject.DefinitionDigest,
		Status: run.Status, QueuedAt: run.QueuedAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
		Samples: run.Samples, HiddenResults: run.HiddenResults, Cost: apiCost(run.Usage, run.Exponent), Reason: run.Reason,
	}
}
