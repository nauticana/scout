package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type pilotBackendFake struct {
	actor  domain.StudioActor
	agent  string
	change domain.PilotChange
	err    error
}

var pilotStart = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func (backend *pilotBackendFake) List(_ context.Context, actor domain.StudioActor, agentID string) ([]domain.PilotMember, error) {
	backend.actor, backend.agent = actor, agentID
	return nil, backend.err
}

func (backend *pilotBackendFake) Add(_ context.Context, actor domain.StudioActor, change domain.PilotChange) (domain.PilotMember, error) {
	backend.actor, backend.change = actor, change
	return domain.PilotMember{AgentID: change.AgentID, Version: change.Version, Subject: change.Subject, Reason: change.Reason,
		AddedBy: actor.Principal, ValidFrom: pilotStart}, backend.err
}

func (backend *pilotBackendFake) Remove(_ context.Context, actor domain.StudioActor, change domain.PilotChange) (domain.PilotMember, error) {
	backend.actor, backend.change = actor, change
	return domain.PilotMember{AgentID: change.AgentID, Version: change.Version, Subject: change.Subject,
		ValidFrom: pilotStart, ValidTo: pilotStart.Add(time.Hour)}, backend.err
}

func pilotHandler(backend *pilotBackendFake) *StudioPilotHandler {
	return &StudioPilotHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
}

// pilotData decodes the data of keel's response envelope.
func pilotData(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	envelope := struct {
		Data json.RawMessage `json:"data"`
	}{}
	if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &envelope) != nil || json.Unmarshal(envelope.Data, target) != nil {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestStudioPilotAddActsAsTheSessionPerson(t *testing.T) {
	configureConversationRequestLimit(t)
	backend := &pilotBackendFake{}
	recorder := httptest.NewRecorder()
	body := []byte(`{"agent_name":"writer-a","version":"4","subject_kind":"` + string(domain.PilotPosition) + `","subject_id":"emea/sales-lead","reason":"pilot"}`)
	pilotHandler(backend).Routes()[api.StudioPilotAddPath](recorder, authenticatedRequest(http.MethodPost, api.StudioPilotAddPath, body))
	want := domain.PilotChange{AgentID: "writer-a", Version: "4", Reason: "pilot", Subject: domain.PilotSubject{Kind: domain.PilotPosition, ID: "emea/sales-lead"}}
	if backend.change != want || backend.actor.TenantID != 7 || backend.actor.Principal != (domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}) {
		t.Fatalf("change = %+v, actor = %+v", backend.change, backend.actor)
	}
	var member map[string]any
	pilotData(t, recorder, &member)
	if member["subject_kind"] != string(domain.PilotPosition) || member["added_by_id"] != "42" || member["agent_name"] != "writer-a" {
		t.Fatalf("member = %v", member)
	}
	if _, ended := member["valid_to"]; ended {
		t.Fatalf("an open membership reported an end: %v", member)
	}
}

func TestStudioPilotRemoveReportsTheEnd(t *testing.T) {
	configureConversationRequestLimit(t)
	recorder := httptest.NewRecorder()
	body := []byte(`{"agent_name":"writer-a","version":"4","subject_kind":"` + string(domain.PilotPrincipal) + `","subject_id":"1001","reason":"done"}`)
	pilotHandler(&pilotBackendFake{}).Routes()[api.StudioPilotRemovePath](recorder, authenticatedRequest(http.MethodPost, api.StudioPilotRemovePath, body))
	var member api.PilotMember
	pilotData(t, recorder, &member)
	if member.ValidTo == nil || !member.ValidTo.Equal(pilotStart.Add(time.Hour)) {
		t.Fatalf("member = %+v", member)
	}
}

func TestStudioPilotMapsRefusals(t *testing.T) {
	configureConversationRequestLimit(t)
	for err, status := range map[error]int{domain.ErrConflict: http.StatusConflict, domain.ErrForbidden: http.StatusForbidden, domain.ErrNotFound: http.StatusNotFound, domain.ErrValidation: http.StatusBadRequest} {
		recorder := httptest.NewRecorder()
		body := []byte(`{"agent_name":"writer-a","version":"4","subject_kind":"` + string(domain.PilotPrincipal) + `","subject_id":"1001","reason":"pilot"}`)
		pilotHandler(&pilotBackendFake{err: err}).Routes()[api.StudioPilotAddPath](recorder, authenticatedRequest(http.MethodPost, api.StudioPilotAddPath, body))
		if recorder.Code != status {
			t.Fatalf("%v: status = %d, want %d", err, recorder.Code, status)
		}
	}
}

func TestStudioPilotListReadsTheAgentName(t *testing.T) {
	backend := &pilotBackendFake{}
	recorder := httptest.NewRecorder()
	pilotHandler(backend).Routes()[api.StudioPilotPath](recorder, authenticatedRequest(http.MethodGet, api.StudioPilotPath+"?agent_name=writer-a", nil))
	var members []api.PilotMember
	pilotData(t, recorder, &members)
	if backend.agent != "writer-a" || backend.actor.TenantID != 7 || members == nil || len(members) != 0 {
		t.Fatalf("agent = %q, actor = %+v, members = %#v; an agent with no pilot lists an empty array", backend.agent, backend.actor, members)
	}
}

func TestStudioPilotFeatureCoversItsRoutes(t *testing.T) {
	handler := &StudioPilotHandler{}
	routes := handler.Routes()
	for _, path := range []string{api.StudioPilotPath, api.StudioPilotAddPath, api.StudioPilotRemovePath} {
		if routes[path] == nil {
			t.Fatalf("missing pilot route %s", path)
		}
	}
	if feature := handler.Feature(); feature.ID != StudioFeaturePilot || len(routes) != 3 ||
		len(feature.Actions) != 2 || feature.Actions[0] != "VIEW" || feature.Actions[1] != "PUBLISH" {
		t.Fatalf("feature = %+v, routes = %d", feature, len(routes))
	}
}
