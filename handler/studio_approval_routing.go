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

// StudioFeatureApprovalRouting is the Studio feature StudioApprovalRoutingHandler serves.
const StudioFeatureApprovalRouting = "approval_routing"

// StudioApprovalRoutingHandler serves an agent's effective approval routing; routes are edited
// through the settings route as the approval_routing setting.
type StudioApprovalRoutingHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioApprovalRoutingBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts this route.
func (h *StudioApprovalRoutingHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureApprovalRouting, Version: 1, Actions: []string{string(domain.StudioView)}}
}

// Routes returns the approval routing route: ?agent_name=.
func (h *StudioApprovalRoutingHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{api.StudioApprovalRoutingPath: query(&h.AbstractHandler, h.routing)}
}

func (h *StudioApprovalRoutingHandler) routing(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	view, err := h.Backend.ApprovalRouting(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	result := api.AgentApprovalRouting{
		AgentName: view.AgentID, Routes: make([]api.ApprovalRouteView, 0, len(view.Routes)),
		Coverage: make([]api.ApprovalCoverageView, 0, len(view.Coverage)), ResolverComposed: view.ResolverComposed, Problems: view.Problems,
	}
	for _, origin := range view.Routes {
		route := origin.Route
		item := api.ApprovalRouteView{
			ID: route.ID, Approver: apiReference(route.Approver), DeadlineSeconds: route.DeadlineSeconds,
			OnExhausted: string(route.OnExhausted), ScopeID: origin.ScopeID, Inherited: origin.Inherited,
			Match: api.ApprovalRouteMatch{Tools: route.Match.Tools, MinRiskTier: string(route.Match.MinRiskTier)},
		}
		for _, source := range route.Match.Sources {
			item.Match.Sources = append(item.Match.Sources, string(source))
		}
		for _, step := range route.Escalation {
			escalation := api.ApprovalEscalationView{ExtensionSeconds: step.ExtensionSeconds}
			if step.Approver != nil {
				reference := apiReference(*step.Approver)
				escalation.Approver = &reference
			}
			item.Escalation = append(item.Escalation, escalation)
		}
		result.Routes = append(result.Routes, item)
	}
	for _, ask := range view.Coverage {
		result.Coverage = append(result.Coverage, api.ApprovalCoverageView{
			ToolID: ask.ToolID, Source: string(ask.Source), RiskTier: string(ask.RiskTier), RouteID: ask.RouteID,
		})
	}
	return result, nil
}

func apiReference(reference domain.ApproverReference) api.ApproverReference {
	return api.ApproverReference{Kind: string(reference.Kind), Ref: reference.Ref}
}
