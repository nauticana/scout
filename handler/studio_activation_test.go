package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type activationBackendFake struct {
	agentID string
	cancel  domain.ReleaseActivationCancel
	items   []domain.ReleaseActivation
	err     error
}

func (f *activationBackendFake) Activations(_ context.Context, _ domain.StudioActor, agentID string) ([]domain.ReleaseActivation, error) {
	f.agentID = agentID
	return f.items, f.err
}

func (f *activationBackendFake) Cancel(_ context.Context, _ domain.StudioActor, request domain.ReleaseActivationCancel) (domain.ReleaseActivation, error) {
	f.cancel = request
	if f.err != nil {
		return domain.ReleaseActivation{}, f.err
	}
	return f.items[0], nil
}

func TestStudioActivationRoutesAndFeature(t *testing.T) {
	h := &StudioActivationHandler{}
	routes := h.Routes()
	if routes[api.StudioActivationsPath] == nil || routes[api.StudioActivationCancelPath] == nil || len(routes) != 2 {
		t.Fatalf("routes = %v", routes)
	}
	if feature := h.Feature(); feature.ID != StudioFeatureActivation || strings.Join(feature.Actions, ",") != "VIEW,PUBLISH" {
		t.Fatalf("feature = %+v", feature)
	}
}

func TestStudioActivationListAndCancelMapTheWireShape(t *testing.T) {
	effective := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	backend := &activationBackendFake{items: []domain.ReleaseActivation{{
		ID: 41, AgentID: "writer-a", Version: "4", EffectiveAt: effective, State: domain.ActivationRefused,
		ScheduledBy: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}, Reason: domain.ActivationGuardRefused, Detail: "gate failed",
		DecidedBy: &domain.PrincipalRef{Kind: domain.PrincipalService, ID: "ops"},
	}}}
	h := &StudioActivationHandler{Backend: backend}
	result, err := h.list(context.Background(), domain.StudioActor{}, url.Values{"agent_name": {"writer-a"}})
	if err != nil || backend.agentID != "writer-a" {
		t.Fatalf("list error = %v, agent = %q", err, backend.agentID)
	}
	encoded, _ := json.Marshal(result)
	for _, want := range []string{`"id":41`, `"version":4`, `"state":"` + string(domain.ActivationRefused) + `"`, `"reason":"` + string(domain.ActivationGuardRefused) + `"`, `"scheduled_by":"human:9"`,
		`"decided_by":"service:ops"`, `"effective_at":"2026-11-01T08:00:00Z"`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("list JSON %s lacks %s", encoded, want)
		}
	}

	if _, err = h.cancel(context.Background(), domain.StudioActor{}, api.ReleaseActivationCancel{AgentName: "writer-a"}); err == nil {
		t.Fatal("a cancel without an activation id must be a bad request")
	}
	if _, err = h.cancel(context.Background(), domain.StudioActor{}, api.ReleaseActivationCancel{AgentName: "writer-a", ActivationID: 41, Reason: "moved"}); err != nil {
		t.Fatal(err)
	}
	if backend.cancel != (domain.ReleaseActivationCancel{AgentID: "writer-a", ActivationID: 41, Reason: "moved"}) {
		t.Fatalf("cancel request = %+v", backend.cancel)
	}
	backend.err = domain.ErrConflict
	if _, err = h.cancel(context.Background(), domain.StudioActor{}, api.ReleaseActivationCancel{AgentName: "writer-a", ActivationID: 41}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("cancel conflict = %v", err)
	}
}

func TestReleaseCarriesItsPendingActivation(t *testing.T) {
	effective := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	release, err := apiRelease(domain.AgentRelease{AgentID: "writer-a", Version: "4", Activation: &domain.ReleaseActivation{
		ID: 41, AgentID: "writer-a", Version: "4", EffectiveAt: effective, State: domain.ActivationPending,
	}})
	if err != nil || release.Active || release.Activation == nil || release.Activation.ID != 41 || !release.Activation.EffectiveAt.Equal(effective) {
		t.Fatalf("release = %+v, err = %v", release, err)
	}
	var request api.AgentPublishRequest
	if err = json.Unmarshal([]byte(`{"agent_name":"writer-a","effective_at":"2026-11-01T09:00:00+01:00"}`), &request); err != nil ||
		request.EffectiveAt == nil || !request.EffectiveAt.Equal(effective) {
		t.Fatalf("effective_at = %v, err = %v", request.EffectiveAt, err)
	}
}

type publishBackendFake struct {
	contract.AgentStudioHTTPBackend
	request domain.AgentPublishRequest
}

func (f *publishBackendFake) Publish(_ context.Context, _ domain.StudioActor, request domain.AgentPublishRequest) (domain.AgentRelease, error) {
	f.request = request
	return domain.AgentRelease{AgentID: request.AgentID, Version: "4"}, nil
}

func TestPublishPassesEffectiveAtThrough(t *testing.T) {
	backend := &publishBackendFake{}
	h := &StudioHandler{Backend: backend}
	effective := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	if _, err := h.publish(context.Background(), domain.StudioActor{}, api.AgentPublishRequest{AgentName: "writer-a", EffectiveAt: &effective}); err != nil {
		t.Fatal(err)
	}
	if !backend.request.EffectiveAt.Equal(effective) {
		t.Fatalf("effective_at = %v", backend.request.EffectiveAt)
	}
	if _, err := h.publish(context.Background(), domain.StudioActor{}, api.AgentPublishRequest{AgentName: "writer-a"}); err != nil || !backend.request.EffectiveAt.IsZero() {
		t.Fatalf("an omitted effective_at publishes now: %v, %v", backend.request.EffectiveAt, err)
	}
}
