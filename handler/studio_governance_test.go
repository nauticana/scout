package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/controlplane"
)

type governanceBackendFake struct {
	actor  domain.StudioActor
	agent  string
	change *domain.AgentGovernanceChange
	view   domain.AgentGovernanceView
	err    error
}

func (f *governanceBackendFake) Governance(_ context.Context, actor domain.StudioActor, agentID string) (domain.AgentGovernanceView, error) {
	f.actor, f.agent = actor, agentID
	return f.view, f.err
}

func (f *governanceBackendFake) SaveGovernance(_ context.Context, actor domain.StudioActor, change domain.AgentGovernanceChange) (domain.AgentGovernanceView, error) {
	f.actor, f.change = actor, &change
	return f.view, f.err
}

func governanceHandler(backend *governanceBackendFake) http.HandlerFunc {
	handler := &StudioGovernanceHandler{UserService: conversationUsers{}, Backend: backend}
	return handler.Routes()[api.StudioGovernancePath]
}

func TestStudioGovernanceServesOnePathForBothMethods(t *testing.T) {
	handler := &StudioGovernanceHandler{}
	if routes := handler.Routes(); len(routes) != 1 || routes[api.StudioGovernancePath] == nil {
		t.Fatalf("routes = %v", routes)
	}
	if feature := handler.Feature(); feature.ID != StudioFeatureGovernance || !slices.Equal(feature.Actions, []string{"VIEW", "EDIT"}) {
		t.Fatalf("feature = %+v", feature)
	}
	recorder := httptest.NewRecorder()
	governanceHandler(&governanceBackendFake{})(recorder, authenticatedRequest(http.MethodPut, api.StudioGovernancePath, []byte(`{}`)))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status = %d", recorder.Code)
	}
}

func TestStudioGovernanceGetReturnsCharterFragments(t *testing.T) {
	backend := &governanceBackendFake{view: domain.AgentGovernanceView{
		AgentID: "writer-a", DraftRevision: 4, Link: "https://charter.acme.example/agents/AGENTDEF-BILLING",
		Effective: domain.AgentGovernance{Purpose: "Resolve billing disputes", ResponsibilityIDs: []domain.CharterRef{{ID: "RESP-1"}}, Triggers: []string{"t"}},
		Draft:     domain.AgentGovernance{ResponsibilityIDs: []domain.CharterRef{{ID: "RESP-1"}}},
	}}
	recorder := httptest.NewRecorder()
	governanceHandler(backend)(recorder, authenticatedRequest(http.MethodGet, api.StudioGovernancePath+"?agent_name=writer-a", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if backend.agent != "writer-a" || backend.actor.TenantID != 7 || backend.actor.Principal.ID != "42" {
		t.Fatalf("backend saw %q as %+v", backend.agent, backend.actor)
	}
	var response struct {
		Data api.AgentGovernance `json:"data"`
	}
	if err := common.DecodeJSON(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	got := response.Data
	if got.AgentName != "writer-a" || got.ExpectedAgentRevision != 4 || got.EnterpriseLink == "" || got.Problems == nil ||
		string(got.Governance) != `{"purpose":"Resolve billing disputes","responsibilityIds":["RESP-1"],"triggers":["t"]}` ||
		string(got.Draft) != `{"responsibilityIds":["RESP-1"]}` {
		t.Fatalf("response = %s", recorder.Body.String())
	}
}

func TestStudioGovernancePostDecodesTheDraftStrictly(t *testing.T) {
	backend := &governanceBackendFake{view: domain.AgentGovernanceView{AgentID: "writer-a", DraftRevision: 5}}
	body := `{"agent_name":"writer-a","expected_agent_revision":4,"draft":{"purpose":"Answer",` +
		`"capabilityIds":[{"namespace":"acme.example","id":"CAP-1"}],"triggerChannels":[{"trigger":"nightly","channel":"schedule","binding":"sched-17"}]}}`
	recorder := httptest.NewRecorder()
	governanceHandler(backend)(recorder, authenticatedRequest(http.MethodPost, api.StudioGovernancePath, []byte(body)))
	if recorder.Code != http.StatusOK || backend.change == nil {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	change := backend.change
	if change.AgentID != "writer-a" || change.ExpectedDraftRevision != 4 || change.Governance.Purpose != "Answer" ||
		change.Governance.CapabilityIDs[0] != (domain.CharterRef{Namespace: "acme.example", ID: "CAP-1"}) ||
		change.Governance.TriggerChannels[0].Binding != "sched-17" || backend.actor.Principal.ID != "42" {
		t.Fatalf("change = %+v", change)
	}

	for name, body := range map[string]string{
		"misspelled member": `{"agent_name":"writer-a","expected_agent_revision":4,"draft":{"responsibilities":["RESP-1"]}}`,
		"missing draft":     `{"agent_name":"writer-a","expected_agent_revision":4}`,
		"null draft":        `{"agent_name":"writer-a","expected_agent_revision":4,"draft":null}`,
		"malformed ref":     `{"agent_name":"writer-a","expected_agent_revision":4,"draft":{"capabilityIds":[7]}}`,
	} {
		refused := &governanceBackendFake{}
		recorder = httptest.NewRecorder()
		governanceHandler(refused)(recorder, authenticatedRequest(http.MethodPost, api.StudioGovernancePath, []byte(body)))
		if recorder.Code != http.StatusBadRequest || refused.change != nil {
			t.Fatalf("%s: status = %d, saved = %v", name, recorder.Code, refused.change != nil)
		}
	}
}

func TestStudioGovernanceMapsServiceRefusals(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		detail string
	}{
		{&controlplane.StudioValidationError{Fields: []domain.AgentFieldError{{Field: "purpose", Message: "owned by the enterprise source"}}}, http.StatusBadRequest, "owned by the enterprise source"},
		{domain.ErrRevisionConflict, http.StatusConflict, ""},
		{domain.ErrForbidden, http.StatusForbidden, ""},
	} {
		recorder := httptest.NewRecorder()
		body := []byte(`{"agent_name":"writer-a","expected_agent_revision":4,"draft":{}}`)
		governanceHandler(&governanceBackendFake{err: test.err})(recorder, authenticatedRequest(http.MethodPost, api.StudioGovernancePath, body))
		if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.detail) {
			t.Fatalf("%v: status = %d, body = %s", test.err, recorder.Code, recorder.Body.String())
		}
	}
}
