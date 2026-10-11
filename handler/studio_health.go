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

// StudioFeatureHealth is the Studio feature StudioHealthHandler serves.
const StudioFeatureHealth = "health"

// StudioHealthHandler serves an agent's production health detail and its alerts.
type StudioHealthHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioHealthBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioHealthHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureHealth, Version: 1, Actions: []string{string(domain.StudioView)}}
}

// Routes returns GET health?agent_name= and GET alerts?agent_name=&status=&before=&limit=.
func (h *StudioHealthHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioHealthPath: query(&h.AbstractHandler, h.health),
		api.StudioAlertsPath: query(&h.AbstractHandler, h.alerts),
	}
}

func (h *StudioHealthHandler) health(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	report, err := h.Backend.Health(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	measure := report.Measure
	result := api.AgentHealth{
		AgentName: report.AgentID, WindowMinutes: measure.WindowMinutes, MeasuredAt: measure.MeasuredAt, Telemetry: string(report.Telemetry),
		TurnsByStatus: make(map[string]int64, len(measure.TurnsByStatus)), Samples: measure.Judged(), InsufficientSamples: report.InsufficientSamples,
		DurationP50MS: measure.DurationP50MS, DurationP95MS: measure.DurationP95MS,
		FirstResponseP50MS: measure.FirstResponseP50MS, FirstResponseP95MS: measure.FirstResponseP95MS,
		LastCompletedAt: optionalTime(measure.LastCompletedAt), LastFailedAt: optionalTime(measure.LastFailedAt),
		SafetyEvents: make(map[string]int64, len(measure.SafetyBySeverity)), OpenAlerts: apiAlerts(report.OpenAlerts),
		PolicyProblem: report.PolicyProblem, LastEvaluatedAt: optionalTime(report.LastEvaluatedAt),
	}
	for status, count := range measure.TurnsByStatus {
		result.TurnsByStatus[string(status)] = count
	}
	for severity, count := range measure.SafetyBySeverity {
		result.SafetyEvents[string(severity)] = count
	}
	if rate, judged := measure.FailureRateBP(); judged {
		result.FailureRateBP = &rate
	}
	if policy := report.Policy; policy != nil {
		result.Policy = &api.HealthAlertPolicy{
			WindowMinutes: policy.WindowMinutes, MinTurns: policy.MinTurns, MaxFailureRateBP: policy.MaxFailureRateBP,
			MaxP95LatencyMS: policy.MaxP95LatencyMS, MaxHardSafetyEvents: policy.MaxHardSafetyEvents, MaxSilenceMinutes: policy.MaxSilenceMinutes,
		}
	}
	return result, nil
}

func (h *StudioHealthHandler) alerts(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	before, beforeErr := optionalInt(query.Get("before"), 0)
	limit, limitErr := optionalInt(query.Get("limit"), 0)
	if beforeErr != nil || limitErr != nil {
		return nil, badRequest("before and limit must be integers")
	}
	page, err := h.Backend.Alerts(ctx, actor, domain.AgentAlertFilter{
		AgentID: query.Get("agent_name"), Status: domain.AgentAlertStatus(query.Get("status")), Before: before, Limit: int(limit),
	})
	if err != nil {
		return nil, err
	}
	return api.AgentAlertPage{Alerts: apiAlerts(page.Alerts), NextBefore: page.NextBefore}, nil
}

func apiAlerts(alerts []domain.AgentAlert) []api.AgentAlert {
	result := make([]api.AgentAlert, 0, len(alerts))
	for _, alert := range alerts {
		result = append(result, api.AgentAlert{
			ID: alert.ID, Kind: string(alert.Kind), Status: string(alert.Status), OpenedAt: alert.OpenedAt, ResolvedAt: optionalTime(alert.ResolvedAt),
			Observed: alert.Observed, Threshold: alert.Threshold, WindowMinutes: alert.WindowMinutes,
		})
	}
	return result
}
