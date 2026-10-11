package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func guardrailsHandler(backend fake.StudioGuardrailBackend) *StudioGuardrailsHandler {
	return &StudioGuardrailsHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
}

// envelopeData decodes the data member of keel's response envelope.
func envelopeData(recorder *httptest.ResponseRecorder, target any) error {
	return json.Unmarshal(recorder.Body.Bytes(), &struct {
		Data any `json:"data"`
	}{Data: target})
}

func serve(handler *StudioGuardrailsHandler, method, target, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	path, _, _ := strings.Cut(target, "?")
	handler.Routes()[path](recorder, authenticatedRequest(method, target, []byte(body)))
	return recorder
}

func TestStudioGuardrailsShowAndReplaceOnOnePath(t *testing.T) {
	var actors []domain.StudioActor
	var saved domain.AgentGuardrailChange
	view := domain.AgentGuardrails{
		AgentID: "writer-a", DraftRevision: 5, Problem: "tenant layer: authority exceeded",
		Inherited: []domain.InheritedGuardrails{{Layer: domain.GuardrailLayerTenant, Digest: "abc", Rules: []domain.GuardrailRule{{ID: "pii", Kind: domain.GuardrailKindPII, Action: domain.GuardrailActionRedact, Severity: domain.GuardrailSeverityHard}}}},
		Rules: []domain.GuardrailRule{{ID: "size", Kind: domain.GuardrailKindMaxOutputBytes, Stages: []domain.GuardrailStage{domain.GuardrailStageOutput},
			Action: domain.GuardrailActionBlock, Severity: domain.GuardrailSeverityHard, Params: json.RawMessage(`{"max":500}`)}},
	}
	handler := guardrailsHandler(fake.StudioGuardrailBackend{
		GuardrailsFunc: func(_ context.Context, actor domain.StudioActor, agentID string) (domain.AgentGuardrails, error) {
			actors = append(actors, actor)
			if agentID != "writer-a" {
				return domain.AgentGuardrails{}, domain.ErrNotFound
			}
			return view, nil
		},
		SaveGuardrailsFunc: func(_ context.Context, actor domain.StudioActor, change domain.AgentGuardrailChange) (domain.AgentGuardrails, error) {
			actors, saved = append(actors, actor), change
			return view, nil
		},
	})

	shown := serve(handler, http.MethodGet, api.StudioGuardrailsPath+"?agent_name=writer-a", "")
	var body api.AgentGuardrails
	if shown.Code != http.StatusOK || envelopeData(shown, &body) != nil {
		t.Fatalf("GET = %d %s", shown.Code, shown.Body.String())
	}
	if body.AgentName != "writer-a" || body.ExpectedAgentRevision != 5 || body.Problem == "" || body.Inherited[0].Layer != "tenant" ||
		body.Inherited[0].Digest != "abc" || body.Rules[0].Stages[0] != "output" || string(body.Rules[0].Params) != `{"max":500}` {
		t.Fatalf("GET body = %s", shown.Body.String())
	}

	replaced := serve(handler, http.MethodPost, api.StudioGuardrailsPath,
		`{"agent_name":"writer-a","expected_agent_revision":4,"rules":[{"id":"size","kind":"max_output_bytes","stages":["output"],"action":"block","severity":"hard","params":{"max":400}}]}`)
	if replaced.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", replaced.Code, replaced.Body.String())
	}
	rule := saved.Rules[0]
	if saved.AgentID != "writer-a" || saved.ExpectedDraftRevision != 4 || rule.Kind != domain.GuardrailKindMaxOutputBytes ||
		rule.Stages[0] != domain.GuardrailStageOutput || string(rule.Params) != `{"max":400}` {
		t.Fatalf("saved = %+v", saved)
	}
	for _, actor := range actors {
		if actor != (domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}}) {
			t.Fatalf("the actor comes from the session: %+v", actor)
		}
	}
	if missing := serve(handler, http.MethodGet, api.StudioGuardrailsPath+"?agent_name=other", ""); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown agent = %d", missing.Code)
	}
	refused := serve(handler, http.MethodPut, api.StudioGuardrailsPath, "{}")
	if refused.Code != http.StatusMethodNotAllowed || refused.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("PUT = %d, Allow %q", refused.Code, refused.Header().Get("Allow"))
	}
}

func TestStudioGuardrailsMapRefusals(t *testing.T) {
	refusal := domain.ErrAuthorityExceeded
	handler := guardrailsHandler(fake.StudioGuardrailBackend{
		SaveGuardrailsFunc: func(context.Context, domain.StudioActor, domain.AgentGuardrailChange) (domain.AgentGuardrails, error) {
			return domain.AgentGuardrails{}, refusal
		},
	})
	body := `{"agent_name":"writer-a","expected_agent_revision":4,"rules":[]}`
	for err, status := range map[error]int{
		domain.ErrAuthorityExceeded: http.StatusForbidden,
		domain.ErrRevisionConflict:  http.StatusConflict,
		domain.ErrValidation:        http.StatusBadRequest,
		domain.ErrNotReady:          http.StatusServiceUnavailable,
	} {
		refusal = err
		if recorder := serve(handler, http.MethodPost, api.StudioGuardrailsPath, body); recorder.Code != status {
			t.Fatalf("%v = %d, want %d", err, recorder.Code, status)
		}
	}
}

func TestStudioSafetyEventsPageWithOnlyStoredEvidence(t *testing.T) {
	var asked domain.SafetyEventFilter
	at := time.Unix(1000, 0).UTC()
	handler := guardrailsHandler(fake.StudioGuardrailBackend{
		SafetyEventsFunc: func(_ context.Context, actor domain.StudioActor, filter domain.SafetyEventFilter) ([]domain.SafetyEvent, error) {
			asked = filter
			if actor.TenantID != 7 {
				t.Fatalf("actor = %+v", actor)
			}
			return []domain.SafetyEvent{{
				ID: 41, TenantID: 7, AgentID: "writer-a", RequestID: "r-1", Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer-a"},
				Stage: domain.GuardrailStageOutput, Layer: domain.GuardrailLayerRelease, Action: domain.GuardrailActionRedact,
				Severity: domain.GuardrailSeverityHard, RuleIDs: []string{"pii"}, ReleaseVersion: "3", PolicyVersion: "g1",
				Duration: 7 * time.Millisecond, OccurredAt: at,
			}}, nil
		},
	})
	recorder := serve(handler, http.MethodGet, api.StudioSafetyEventsPath+"?agent_name=writer-a&before=50&limit=20", "")
	var page []api.SafetyEvent
	if recorder.Code != http.StatusOK || envelopeData(recorder, &page) != nil {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body.String())
	}
	if asked != (domain.SafetyEventFilter{AgentID: "writer-a", Before: 50, Limit: 20}) {
		t.Fatalf("filter = %+v", asked)
	}
	event := page[0]
	if len(page) != 1 || event.ID != 41 || event.Version != "3" || event.PolicyVersion != "g1" || event.ActorKind != "agent" ||
		event.RuleIDs[0] != "pii" || event.DurationMs != 7 || event.RequestID != "r-1" || !event.OccurredAt.Equal(at) {
		t.Fatalf("page = %s", recorder.Body.String())
	}
	if serve(handler, http.MethodGet, api.StudioSafetyEventsPath+"?agent_name=writer-a", ""); asked.Limit != 50 || asked.Before != 0 {
		t.Fatalf("the first page defaults to 50 events: %+v", asked)
	}
	if bad := serve(handler, http.MethodGet, api.StudioSafetyEventsPath+"?agent_name=writer-a&before=x", ""); bad.Code != http.StatusBadRequest {
		t.Fatalf("a malformed cursor = %d", bad.Code)
	}
}

func TestStudioGuardrailsFeatureAndRoutes(t *testing.T) {
	handler := &StudioGuardrailsHandler{}
	feature := handler.Feature()
	if feature.ID != StudioFeatureGuardrails || feature.Version != 1 || strings.Join(feature.Actions, ",") != "VIEW,EDIT" {
		t.Fatalf("feature = %+v", feature)
	}
	routes := handler.Routes()
	if len(routes) != 2 || routes[api.StudioGuardrailsPath] == nil || routes[api.StudioSafetyEventsPath] == nil {
		t.Fatalf("routes = %v", routes)
	}
}
