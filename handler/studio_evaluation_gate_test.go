package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type gateBackendFake struct {
	actor    domain.StudioActor
	agentID  string
	override domain.EvaluationGateOverride
	err      error
}

func (f *gateBackendFake) state(agentID string) domain.EvaluationGateState {
	regression := int64(0)
	return domain.EvaluationGateState{AgentID: agentID, CurrentDigest: "d1", PolicyDigest: "p1", SetVersion: 3,
		Policy: &domain.EvaluationGatePolicy{Metric: "rubric_score", MinScoreBP: 8000, MaxRegressionBP: &regression,
			RequiredScopes: []domain.GoldenScope{domain.GoldenScopeDev}},
		Decision: &domain.EvaluationGateDecision{DefinitionDigest: "d1", Verdict: domain.GateVerdictOverride, Reason: "hotfix",
			DecidedBy: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"},
			Runs:      []domain.EvaluationGateRun{{RunID: 41, Role: domain.GateRunCandidate, Scope: domain.GoldenScopeDev, ScoreBP: 7000, Samples: 2, Criticals: 1}}},
		Admitted: true}
}

func (f *gateBackendFake) Gate(_ context.Context, actor domain.StudioActor, agentID string) (domain.EvaluationGateState, error) {
	f.actor, f.agentID = actor, agentID
	return f.state(agentID), f.err
}

func (f *gateBackendFake) EvaluateGate(_ context.Context, actor domain.StudioActor, agentID string) (domain.EvaluationGateState, error) {
	f.actor, f.agentID = actor, agentID
	return f.state(agentID), f.err
}

func (f *gateBackendFake) OverrideGate(_ context.Context, actor domain.StudioActor, override domain.EvaluationGateOverride) (domain.EvaluationGateState, error) {
	f.actor, f.override = actor, override
	return f.state(override.AgentID), f.err
}

func TestEvaluationGateRoutesMapTheWire(t *testing.T) {
	backend := &gateBackendFake{}
	handler := &StudioEvaluationGateHandler{UserService: conversationUsers{}, Backend: backend}
	routes := handler.Routes()
	feature := handler.Feature()
	if len(routes) != 3 || feature.ID != StudioFeatureEvaluationGate || !slices.Contains(feature.Actions, string(domain.StudioOverrideGate)) {
		t.Fatalf("routes = %v, feature = %+v", routes, feature)
	}

	recorder := httptest.NewRecorder()
	routes[api.StudioGatePath](recorder, authenticatedRequest(http.MethodGet, api.StudioGatePath+"?agent_name=writer-a", nil))
	var state api.EvaluationGateState
	if err := unwrapData(recorder.Body.Bytes(), &state); err != nil || recorder.Code != http.StatusOK || backend.agentID != "writer-a" {
		t.Fatalf("gate: %d %s", recorder.Code, recorder.Body.String())
	}
	if state.Policy == nil || state.Policy.MaxRegressionBP == nil || *state.Policy.MaxRegressionBP != 0 || state.Stale == nil ||
		state.Decision.Verdict != string(domain.GateVerdictOverride) || state.Decision.DecidedByID != "42" || state.Decision.Runs[0].CriticalFailures != 1 || !state.Admitted {
		t.Fatalf("state = %+v", state)
	}

	recorder = httptest.NewRecorder()
	routes[api.StudioGateOverridePath](recorder, authenticatedRequest(http.MethodPost, api.StudioGateOverridePath,
		[]byte(`{"agent_name":"writer-a","definition_digest":"d1","reason":"hotfix"}`)))
	if recorder.Code != http.StatusOK || backend.override.DefinitionDigest != "d1" || backend.override.Reason != "hotfix" || backend.actor.Principal.ID != "42" {
		t.Fatalf("override: %d %s %+v", recorder.Code, recorder.Body.String(), backend.override)
	}

	backend.err = &domain.EvaluationGateRefusal{Code: domain.GateRefusedFailed, Detail: "dev score is below 8000 bp"}
	recorder = httptest.NewRecorder()
	routes[api.StudioGateEvaluatePath](recorder, authenticatedRequest(http.MethodPost, api.StudioGateEvaluatePath, []byte(`{"agent_name":"writer-a"}`)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("refusal: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	routes[api.StudioGateEvaluatePath](recorder, authenticatedRequest(http.MethodPost, api.StudioGateEvaluatePath, []byte(`{"agent_name":"writer-a","extra":1}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown member: %d", recorder.Code)
	}
}
