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

// StudioFeatureGuardrails is the Studio feature StudioGuardrailsHandler serves.
const StudioFeatureGuardrails = "guardrails"

// StudioGuardrailsHandler serves an agent's enforced guardrail rules and its safety events.
type StudioGuardrailsHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioGuardrailBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioGuardrailsHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureGuardrails, Version: 1, Actions: []string{string(domain.StudioView), string(domain.StudioEdit)}}
}

// Routes returns GET and POST guardrails?agent_name= and GET safety-events?agent_name=&before=&limit=.
func (h *StudioGuardrailsHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioGuardrailsPath:   readOrWrite(&h.AbstractHandler, query(&h.AbstractHandler, h.show), command(&h.AbstractHandler, h.save)),
		api.StudioSafetyEventsPath: query(&h.AbstractHandler, h.safetyEvents),
	}
}

// readOrWrite serves one path with a GET read and a POST write.
func readOrWrite(h *keelhandler.AbstractHandler, read, write http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			read(w, r)
		case http.MethodPost:
			write(w, r)
		default:
			h.RequireMethod(w, r, http.MethodGet, http.MethodPost)
		}
	}
}

func (h *StudioGuardrailsHandler) show(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	return apiGuardrails(h.Backend.Guardrails(ctx, actor, query.Get("agent_name")))
}

func (h *StudioGuardrailsHandler) save(ctx context.Context, actor domain.StudioActor, request api.AgentGuardrailChange) (any, error) {
	return apiGuardrails(h.Backend.SaveGuardrails(ctx, actor, domain.AgentGuardrailChange{
		AgentID: request.AgentName, Rules: domainGuardrailRules(request.Rules), ExpectedDraftRevision: request.ExpectedAgentRevision,
	}))
}

func (h *StudioGuardrailsHandler) safetyEvents(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	before, beforeErr := optionalInt(query.Get("before"), 0)
	limit, limitErr := optionalInt(query.Get("limit"), 50)
	if beforeErr != nil || limitErr != nil {
		return nil, badRequest("before and limit must be integers")
	}
	events, err := h.Backend.SafetyEvents(ctx, actor, domain.SafetyEventFilter{AgentID: query.Get("agent_name"), Before: before, Limit: int(limit)})
	if err != nil {
		return nil, err
	}
	items := make([]api.SafetyEvent, 0, len(events))
	for _, event := range events {
		items = append(items, api.SafetyEvent{
			ID: event.ID, Version: event.ReleaseVersion, PolicyVersion: event.PolicyVersion, Stage: string(event.Stage),
			Layer: string(event.Layer), Action: string(event.Action), Severity: string(event.Severity),
			RuleIDs: append([]string{}, event.RuleIDs...), ActorKind: string(event.Principal.Kind), ActorID: event.Principal.ID,
			RequestID: event.RequestID, DurationMs: event.Duration.Milliseconds(), OccurredAt: event.OccurredAt,
		})
	}
	return items, nil
}

func apiGuardrails(guardrails domain.AgentGuardrails, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	result := api.AgentGuardrails{
		AgentName: guardrails.AgentID, Inherited: make([]api.InheritedGuardrails, 0, len(guardrails.Inherited)),
		Rules: apiGuardrailRules(guardrails.Rules), Problem: guardrails.Problem, ExpectedAgentRevision: guardrails.DraftRevision,
	}
	for _, layer := range guardrails.Inherited {
		result.Inherited = append(result.Inherited, api.InheritedGuardrails{Layer: string(layer.Layer), Digest: layer.Digest, Rules: apiGuardrailRules(layer.Rules)})
	}
	return result, nil
}

func apiGuardrailRules(rules []domain.GuardrailRule) []api.GuardrailRule {
	items := make([]api.GuardrailRule, 0, len(rules))
	for _, rule := range rules {
		item := api.GuardrailRule{ID: rule.ID, Kind: string(rule.Kind), Action: string(rule.Action), Severity: string(rule.Severity), Params: rule.Params}
		for _, stage := range rule.Stages {
			item.Stages = append(item.Stages, string(stage))
		}
		items = append(items, item)
	}
	return items
}

func domainGuardrailRules(rules []api.GuardrailRule) []domain.GuardrailRule {
	items := make([]domain.GuardrailRule, 0, len(rules))
	for _, rule := range rules {
		item := domain.GuardrailRule{
			ID: rule.ID, Kind: domain.GuardrailRuleKind(rule.Kind), Action: domain.GuardrailAction(rule.Action),
			Severity: domain.GuardrailSeverity(rule.Severity), Params: rule.Params,
		}
		for _, stage := range rule.Stages {
			item.Stages = append(item.Stages, domain.GuardrailStage(stage))
		}
		items = append(items, item)
	}
	return items
}
