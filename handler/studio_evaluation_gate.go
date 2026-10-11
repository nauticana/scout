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

// StudioFeatureEvaluationGate is the Studio feature StudioEvaluationGateHandler serves.
const StudioFeatureEvaluationGate = "evaluation_gate"

// StudioEvaluationGateHandler serves the evaluation gate of an agent's current draft.
type StudioEvaluationGateHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioEvaluationGateBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioEvaluationGateHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureEvaluationGate, Version: 1, Actions: []string{
		string(domain.StudioView), string(domain.StudioTest), string(domain.StudioOverrideGate)}}
}

// Routes returns GET gate?agent_name=, POST gate/evaluate, and POST gate/override.
func (h *StudioEvaluationGateHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioGatePath:         query(&h.AbstractHandler, h.gate),
		api.StudioGateEvaluatePath: command(&h.AbstractHandler, h.evaluate),
		api.StudioGateOverridePath: command(&h.AbstractHandler, h.override),
	}
}

func (h *StudioEvaluationGateHandler) gate(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	return apiGateState(h.Backend.Gate(ctx, actor, query.Get("agent_name")))
}

func (h *StudioEvaluationGateHandler) evaluate(ctx context.Context, actor domain.StudioActor, request api.EvaluationGateRequest) (any, error) {
	return apiGateState(h.Backend.EvaluateGate(ctx, actor, request.AgentName))
}

func (h *StudioEvaluationGateHandler) override(ctx context.Context, actor domain.StudioActor, request api.EvaluationGateOverrideRequest) (any, error) {
	return apiGateState(h.Backend.OverrideGate(ctx, actor, domain.EvaluationGateOverride{
		AgentID: request.AgentName, DefinitionDigest: request.DefinitionDigest, Reason: request.Reason,
	}))
}

func apiGateState(state domain.EvaluationGateState, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	result := api.EvaluationGateState{
		AgentName: state.AgentID, CurrentDigest: state.CurrentDigest, PolicyDigest: state.PolicyDigest, ActiveVersion: state.ActiveVersion,
		GoldenSetID: state.GoldenSetID, SetVersion: state.SetVersion, Stale: nonNil(state.Stale), Admitted: state.Admitted, Problem: state.Problem,
	}
	if state.Policy != nil {
		policy := apiGatePolicy(*state.Policy)
		result.Policy = &policy
	}
	if decision := state.Decision; decision != nil {
		mapped := api.EvaluationGateDecision{
			DefinitionDigest: decision.DefinitionDigest, Verdict: string(decision.Verdict), Reason: decision.Reason,
			Policy: apiGatePolicy(decision.Policy), PolicyDigest: decision.PolicyDigest, BaseVersion: decision.BaseVersion,
			GoldenSetID: decision.GoldenSetID, SetVersion: decision.SetVersion, Runs: make([]api.EvaluationGateRun, 0, len(decision.Runs)),
			DecidedByKind: string(decision.DecidedBy.Kind), DecidedByID: decision.DecidedBy.ID, DecidedAt: decision.DecidedAt,
		}
		for _, run := range decision.Runs {
			mapped.Runs = append(mapped.Runs, api.EvaluationGateRun{RunID: run.RunID, Role: string(run.Role), Scope: string(run.Scope),
				ScoreBP: run.ScoreBP, Samples: run.Samples, CriticalFailures: run.Criticals})
		}
		result.Decision = &mapped
	}
	return result, nil
}

func apiGatePolicy(policy domain.EvaluationGatePolicy) api.EvaluationGatePolicy {
	scopes := make([]string, 0, len(policy.RequiredScopes))
	for _, scope := range policy.RequiredScopes {
		scopes = append(scopes, string(scope))
	}
	return api.EvaluationGatePolicy{Metric: policy.Metric, MinScoreBP: policy.MinScoreBP, MaxRegressionBP: policy.MaxRegressionBP,
		MinSamples: policy.MinSamples, RequiredScopes: scopes}
}
