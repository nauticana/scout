package modelgateway

import (
	"context"
	"fmt"
	"slices"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// PinnedModelRouter routes a request to the model its release pins, with no
// capacity snapshots. It still goes through the tenant's catalog: a model the
// tenant was not granted, or one lacking a required capability, is ErrNoRoute.
type PinnedModelRouter struct {
	Catalog contract.ModelCandidateCatalog
}

func (router *PinnedModelRouter) Select(ctx context.Context, request domain.ModelRequest) (domain.ModelSelection, error) {
	if router.Catalog == nil {
		return domain.ModelSelection{}, fmt.Errorf("pinned model router: candidate catalog is required")
	}
	pinned := request.Model
	if pinned.ProviderID == "" || pinned.ModelID == "" {
		return domain.ModelSelection{}, fmt.Errorf("%w: the request pins no model", domain.ErrValidation)
	}
	candidates, err := router.Catalog.CandidatesFor(ctx, request.TenantContext)
	if err != nil {
		return domain.ModelSelection{}, fmt.Errorf("route candidates for tenant %d: %w", request.TenantContext.TenantID, err)
	}
	required := RequiredCapabilities(request)
	for _, candidate := range candidates.Candidates {
		if candidate.Provider != pinned.ProviderID || candidate.Model != pinned.ModelID || slices.Contains(request.ExcludedRouteIDs, candidate.RouteID) {
			continue
		}
		if slices.ContainsFunc(required, func(capability string) bool { return !slices.Contains(candidate.Capabilities, capability) }) {
			continue
		}
		return domain.ModelSelection{
			Provider: candidate.Provider, Model: candidate.Model, ModelVersion: candidate.ModelVersion,
			Region: candidate.Region, RouteID: candidate.RouteID, RoutingGeneration: candidates.Generation,
			Reason: "pinned by release",
		}, nil
	}
	return domain.ModelSelection{}, fmt.Errorf("%w: tenant %d has no route for pinned model %s/%s with %q",
		domain.ErrNoRoute, request.TenantContext.TenantID, pinned.ProviderID, pinned.ModelID, required)
}

var _ contract.ModelRouter = (*PinnedModelRouter)(nil)
