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

// StudioFeatureDelegation is the Studio feature StudioDelegationHandler serves.
const StudioFeatureDelegation = "delegation"

// StudioDelegationHandler serves the agent-to-agent delegation grants of Studio.
type StudioDelegationHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioDelegationBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioDelegationHandler) Feature() api.StudioFeature {
	return api.StudioFeature{ID: StudioFeatureDelegation, Version: 1, Actions: []string{string(domain.StudioView), string(domain.StudioEdit)}}
}

// Routes returns the delegation routes.
func (h *StudioDelegationHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioDelegationsPath: query(&h.AbstractHandler, h.list),
		api.StudioDelegatePath:    command(&h.AbstractHandler, h.grant),
		api.StudioUndelegatePath:  command(&h.AbstractHandler, h.revoke),
	}
}

func (h *StudioDelegationHandler) list(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	grants, err := h.Backend.List(ctx, actor, query.Get("agent_name"))
	if err != nil {
		return nil, err
	}
	items := make([]api.DelegationGrant, 0, len(grants))
	for _, grant := range grants {
		items = append(items, apiGrant(grant))
	}
	return items, nil
}

func (h *StudioDelegationHandler) grant(ctx context.Context, actor domain.StudioActor, request api.DelegationGrant) (any, error) {
	grant := domain.DelegationGrant{
		Grantor: domain.PrincipalRef{ID: request.AgentName}, Grantee: domain.PrincipalRef{ID: request.Grantee},
		ActionScope: request.ActionScope, MaxDepth: request.MaxDepth, BudgetMinorUnits: request.BudgetMinorUnits,
		Currency: request.Currency, ApprovalRequired: request.ApprovalRequired, ValidTo: request.ValidTo,
	}
	if request.ValidFrom != nil {
		grant.ValidFrom = *request.ValidFrom
	}
	granted, err := h.Backend.Grant(ctx, actor, grant)
	if err != nil {
		return nil, err
	}
	return apiGrant(granted), nil
}

func (h *StudioDelegationHandler) revoke(ctx context.Context, actor domain.StudioActor, request api.DelegationRevokeRequest) (any, error) {
	return request, h.Backend.Revoke(ctx, actor, request.AgentName, request.GrantID)
}

func apiGrant(grant domain.DelegationGrant) api.DelegationGrant {
	validFrom := grant.ValidFrom
	return api.DelegationGrant{
		GrantID: grant.GrantID, AgentName: grant.Grantor.ID, Grantee: grant.Grantee.ID, ActionScope: grant.ActionScope,
		MaxDepth: grant.MaxDepth, BudgetMinorUnits: grant.BudgetMinorUnits, Currency: grant.Currency,
		ApprovalRequired: grant.ApprovalRequired, ValidFrom: &validFrom, ValidTo: grant.ValidTo,
	}
}
