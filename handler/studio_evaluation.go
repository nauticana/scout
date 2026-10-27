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
	return api.StudioFeature{ID: StudioFeatureEvaluation, Version: 2, Actions: []string{
		string(domain.StudioView), string(domain.StudioEdit), string(domain.StudioTest),
	}}
}

// Routes returns GET cases?agent_name=, POST cases, POST cases/freeze, POST evaluate,
// GET evaluation?run_id=&before=&limit=, and GET evaluation/trajectory?run_id=&case_id=&arm=.
func (h *StudioEvaluationHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioCasesPath:                h.casesRoute(),
		api.StudioCasesFreezePath:          command(&h.AbstractHandler, h.freeze),
		api.StudioEvaluatePath:             command(&h.AbstractHandler, h.evaluate),
		api.StudioEvaluationPath:           query(&h.AbstractHandler, h.evaluation),
		api.StudioEvaluationTrajectoryPath: query(&h.AbstractHandler, h.trajectory),
	}
}

// trajectory reads arm b of a run, or with arm=a arm a of a comparison.
func (h *StudioEvaluationHandler) trajectory(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	runID, err := optionalInt(query.Get("run_id"), 0)
	if err != nil {
		return nil, badRequest("run_id must be an integer")
	}
	arm, role := query.Get("arm"), domain.RoleCandidate
	switch arm {
	case "", "b":
		arm = "b"
	case "a":
		role = domain.RoleBaseline
	default:
		return nil, badRequest("arm must be a or b")
	}
	trajectory, err := h.Backend.Trajectory(ctx, actor, runID, query.Get("case_id"), role)
	if err != nil {
		return nil, err
	}
	result := api.CaseTrajectory{RunID: trajectory.RunID, CaseID: trajectory.ExampleID, Arm: arm, Events: make([]api.TrajectoryEvent, 0, len(trajectory.Events))}
	for _, event := range trajectory.Events {
		item := api.TrajectoryEvent{
			Sequence: event.Sequence, Kind: string(event.Kind), Name: event.Name, Payload: event.Payload,
			OffsetMs: event.Offset.Milliseconds(), Failed: event.Failed, Recovered: event.Recovered,
		}
		item.Usage = trajectoryUsage(event.Usage)
		result.Events = append(result.Events, item)
	}
	return result, nil
}

// caseScript maps a case's API turns and tools onto its script.
func caseScript(goldenCase api.GoldenCase) domain.CaseScript {
	var script domain.CaseScript
	for _, input := range goldenCase.Turns {
		script.Turns = append(script.Turns, domain.CaseTurn{Input: input})
	}
	for _, tool := range goldenCase.Tools {
		script.Tools = append(script.Tools, domain.CaseTool{ToolID: tool.ToolID, Output: tool.Output, IsError: tool.IsError})
	}
	return script
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
			Script: caseScript(request.Case),
			Assurance: domain.AssuranceLabels{
				RequiredEvidence: request.Case.RequiredEvidence, ControllingAuthority: request.Case.ControllingAuthority,
				ExecutionExpected: request.Case.ExecutionExpected,
			},
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
			HasTrajectory: item.Trajectory.URI != "",
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
		item := api.GoldenCase{
			CaseID: goldenCase.CaseID, Name: goldenCase.Name, Input: string(goldenCase.Input), Language: goldenCase.Language,
			ExpectedBehavior: string(goldenCase.ExpectedBehavior), Rubric: goldenCase.RubricRef, RiskTier: goldenCase.RiskTier,
			ModifiedAt: goldenCase.ModifiedAt, ReviewItemID: goldenCase.ReviewItemID, InputWithheld: goldenCase.InputWithheld,
			RequiredEvidence: goldenCase.Assurance.RequiredEvidence, ControllingAuthority: goldenCase.Assurance.ControllingAuthority,
			ExecutionExpected: goldenCase.Assurance.ExecutionExpected,
		}
		for _, turn := range goldenCase.Script.Turns {
			item.Turns = append(item.Turns, turn.Input)
		}
		for _, tool := range goldenCase.Script.Tools {
			item.Tools = append(item.Tools, api.GoldenCaseTool{ToolID: tool.ToolID, Output: tool.Output, IsError: tool.IsError})
		}
		result.Cases = append(result.Cases, item)
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
		Status: string(run.Status), QueuedAt: run.QueuedAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
		Samples: run.Samples, HiddenResults: run.HiddenResults, Cost: apiCost(run.Usage, run.Exponent), Reason: run.Reason,
	}
}
