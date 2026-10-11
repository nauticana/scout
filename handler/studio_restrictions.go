package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureRestrictions is the Studio feature StudioRestrictionHandler serves.
const StudioFeatureRestrictions = "restrictions"

// StudioRestrictionHandler serves the platform's restriction layer, read-only, and the
// tenant's, which is replaced as a whole.
type StudioRestrictionHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioRestrictionBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts this route.
func (h *StudioRestrictionHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureRestrictions, Version: 1, Object: domain.RestrictionAuthorizationObject,
		Actions: []string{string(domain.RestrictionView), string(domain.RestrictionReplaceTenant)}}
}

// Routes returns the restrictions route: GET reads the layers and POST replaces the tenant's.
func (h *StudioRestrictionHandler) Routes() map[string]http.HandlerFunc {
	read, replace := query(&h.AbstractHandler, h.restrictions), command(&h.AbstractHandler, h.replace)
	return map[string]http.HandlerFunc{api.StudioRestrictionsPath: func(w http.ResponseWriter, r *http.Request) {
		if !h.RequireMethod(w, r, http.MethodGet, http.MethodPost) {
			return
		}
		if r.Method == http.MethodGet {
			read(w, r)
			return
		}
		bounded(replace)(w, r)
	}}
}

func (h *StudioRestrictionHandler) restrictions(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	overview, err := h.Backend.Restrictions(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	result := api.StudioRestrictions{
		Platform: apiRestrictionLayer(overview.Layers.Platform), Tenant: apiRestrictionLayer(overview.Layers.Tenant),
		AgentName: overview.AgentID, SettingsProblem: overview.SettingsProblem,
		Overrides: make([]api.RestrictionOverride, 0, len(overview.Overrides)),
	}
	for _, override := range overview.Overrides {
		item := api.RestrictionOverride{
			Layer: string(override.Layer), Kind: string(override.Kind), RestrictionID: override.RestrictionID,
			Conditional: override.Conditional, Settings: make([]api.RestrictedSetting, 0, len(override.Settings)),
		}
		for _, setting := range override.Settings {
			item.Settings = append(item.Settings, api.RestrictedSetting{
				ResourceKind: string(setting.ResourceKind), ResourceID: setting.ResourceID, Members: setting.Members,
			})
		}
		result.Overrides = append(result.Overrides, item)
	}
	return result, nil
}

func (h *StudioRestrictionHandler) replace(ctx context.Context, actor domain.StudioActor, request api.RestrictionReplaceRequest) (any, error) {
	var layer domain.RestrictionLayer
	for _, denial := range request.Denials {
		layer.Denials = append(layer.Denials, domain.PolicyStatement{
			ID: denial.ID, Effect: domain.PolicyDeny, Actions: denial.Actions, Resources: denial.Resources,
			Conditions: absentIfNull(denial.Conditions), Reason: denial.Reason,
		})
	}
	for _, rule := range request.Guardrails {
		var stages []domain.GuardrailStage
		for _, stage := range rule.Stages {
			stages = append(stages, domain.GuardrailStage(stage))
		}
		layer.Guardrails = append(layer.Guardrails, domain.GuardrailRule{
			ID: rule.ID, Kind: domain.GuardrailRuleKind(rule.Kind), Stages: stages, Action: domain.GuardrailAction(rule.Action),
			Severity: domain.GuardrailSeverity(rule.Severity), Params: absentIfNull(rule.Params),
		})
	}
	written, err := h.Backend.ReplaceTenant(ctx, actor, layer, request.ExpectedDigest)
	if err != nil {
		return nil, err
	}
	return apiRestrictionLayer(written), nil
}

func apiRestrictionLayer(layer domain.RestrictionLayer) api.RestrictionLayer {
	result := api.RestrictionLayer{
		Digest: layer.Digest, Denials: make([]api.RestrictionDenial, 0, len(layer.Denials)),
		Guardrails: make([]api.RestrictionGuardrail, 0, len(layer.Guardrails)),
	}
	for _, denial := range layer.Denials {
		result.Denials = append(result.Denials, api.RestrictionDenial{
			ID: denial.ID, Actions: denial.Actions, Resources: denial.Resources, Conditions: denial.Conditions, Reason: denial.Reason,
		})
	}
	for _, rule := range layer.Guardrails {
		var stages []string
		for _, stage := range rule.Stages {
			stages = append(stages, string(stage))
		}
		result.Guardrails = append(result.Guardrails, api.RestrictionGuardrail{
			ID: rule.ID, Kind: string(rule.Kind), Stages: stages, Action: string(rule.Action),
			Severity: string(rule.Severity), Params: rule.Params,
		})
	}
	return result
}

// absentIfNull drops a JSON null, which means the field is absent.
func absentIfNull(raw json.RawMessage) json.RawMessage {
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return raw
}
