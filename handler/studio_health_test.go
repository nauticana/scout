package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func serveHealth(handler *StudioHealthHandler, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	path, _, _ := strings.Cut(target, "?")
	handler.Routes()[path](recorder, authenticatedRequest(http.MethodGet, target, nil))
	return recorder
}

func TestStudioHealthRouteServesTheDetailWithNullsForMissingSamples(t *testing.T) {
	at := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	p95 := int64(4000)
	var asked string
	handler := &StudioHealthHandler{UserService: conversationUsers{}, Backend: &fake.StudioHealthBackend{
		HealthFunc: func(_ context.Context, actor domain.StudioActor, agentID string) (domain.AgentHealthReport, error) {
			asked = agentID
			if actor.TenantID != 7 {
				t.Fatalf("actor = %+v", actor)
			}
			return domain.AgentHealthReport{
				AgentID: agentID, Telemetry: domain.TelemetryFresh, LastEvaluatedAt: at,
				Measure: domain.AgentHealthMeasure{WindowMinutes: 60, MeasuredAt: at, DurationP95MS: &p95, LastCompletedAt: at,
					TurnsByStatus:    map[domain.TurnStatus]int64{domain.TurnCompleted: 18, domain.TurnFailed: 2},
					SafetyBySeverity: map[domain.GuardrailSeverity]int64{domain.GuardrailSeverityHard: 1}},
				Policy:     &domain.HealthAlertPolicy{MinTurns: 10, MaxFailureRateBP: 500},
				OpenAlerts: []domain.AgentAlert{{ID: 9, Kind: domain.AlertFailureRate, Status: domain.AlertOpen, OpenedAt: at, Observed: 1000, Threshold: 500, WindowMinutes: 60}},
			}, nil
		},
	}}
	recorder := serveHealth(handler, api.StudioHealthPath+"?agent_name=writer-a")
	var body api.AgentHealth
	if recorder.Code != http.StatusOK || envelopeData(recorder, &body) != nil || asked != "writer-a" {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body.String())
	}
	if body.Samples != 20 || body.FailureRateBP == nil || *body.FailureRateBP != 1000 || *body.DurationP95MS != 4000 || body.DurationP50MS != nil ||
		body.TurnsByStatus["C"] != 18 || body.SafetyEvents["hard"] != 1 || body.Telemetry != api.TelemetryFresh || body.LastFailedAt != nil ||
		body.Policy == nil || body.Policy.MaxFailureRateBP != 500 || len(body.OpenAlerts) != 1 || body.OpenAlerts[0].Kind != "F" || body.OpenAlerts[0].ResolvedAt != nil {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"duration_p50_ms":null`) {
		t.Fatalf("a percentile without samples is null, not zero: %s", recorder.Body.String())
	}
}

func TestStudioAlertsRoutePassesTheFilterAndRefusesAMalformedCursor(t *testing.T) {
	var asked domain.AgentAlertFilter
	handler := &StudioHealthHandler{UserService: conversationUsers{}, Backend: &fake.StudioHealthBackend{
		AlertsFunc: func(_ context.Context, _ domain.StudioActor, filter domain.AgentAlertFilter) (domain.AgentAlertPage, error) {
			asked = filter
			if filter.Status == "X" {
				return domain.AgentAlertPage{}, domain.ErrValidation
			}
			return domain.AgentAlertPage{Alerts: []domain.AgentAlert{{ID: 3, Kind: domain.AlertSilence, Status: domain.AlertResolved,
				ResolvedAt: time.Unix(2000, 0).UTC()}}, NextBefore: 3}, nil
		},
	}}
	recorder := serveHealth(handler, api.StudioAlertsPath+"?agent_name=writer-a&status=R&before=10&limit=1")
	var page api.AgentAlertPage
	if recorder.Code != http.StatusOK || envelopeData(recorder, &page) != nil {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body.String())
	}
	if asked != (domain.AgentAlertFilter{AgentID: "writer-a", Status: domain.AlertResolved, Before: 10, Limit: 1}) ||
		page.NextBefore != 3 || len(page.Alerts) != 1 || page.Alerts[0].ResolvedAt == nil {
		t.Fatalf("filter = %+v, page = %+v", asked, page)
	}
	if bad := serveHealth(handler, api.StudioAlertsPath+"?agent_name=writer-a&limit=x"); bad.Code != http.StatusBadRequest {
		t.Fatalf("a malformed limit = %d", bad.Code)
	}
	if bad := serveHealth(handler, api.StudioAlertsPath+"?agent_name=writer-a&status=X"); bad.Code != http.StatusBadRequest {
		t.Fatalf("an unknown status = %d", bad.Code)
	}
	feature := handler.Feature()
	if feature.ID != StudioFeatureHealth || feature.Version != 1 || strings.Join(feature.Actions, ",") != "VIEW" || len(handler.Routes()) != 2 {
		t.Fatalf("feature = %+v", feature)
	}
}
