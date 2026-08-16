package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// MediaGateway applies model admission and capacity controls to image and video providers.
type MediaGateway struct {
	RateLimiter contract.TenantRateLimiter
	Provider    contract.MediaProvider
	Capacity    contract.CapacityScheduler
	ProviderID  string
	// Region and RouteID name the media route for capacity scheduling and signals.
	Region  string
	RouteID string
}

// NewMediaGateway builds a governed media provider.
func NewMediaGateway(rateLimiter contract.TenantRateLimiter, provider contract.MediaProvider, capacity contract.CapacityScheduler, providerID string) (*MediaGateway, error) {
	providerID = strings.TrimSpace(providerID)
	if rateLimiter == nil || provider == nil || capacity == nil || providerID == "" {
		return nil, fmt.Errorf("media gateway: rate limiter, provider, capacity scheduler, and provider id are required")
	}
	return &MediaGateway{RateLimiter: rateLimiter, Provider: provider, Capacity: capacity, ProviderID: providerID}, nil
}

func (gateway *MediaGateway) GenerateImage(ctx context.Context, model string, request domain.ImageRequest) ([]domain.GeneratedMedia, error) {
	invocation := domain.ModelRequest{
		TenantContext: request.TenantContext, RequestID: request.RequestID, ConversationID: request.ConversationID,
		Prompt: []byte(request.Prompt), MaxOutputTokens: 1,
	}
	lease, err := gateway.acquire(ctx, model, invocation)
	if err != nil {
		return nil, err
	}
	media, callErr := gateway.Provider.GenerateImage(ctx, model, request)
	return media, errors.Join(callErr, lease.Release(context.WithoutCancel(ctx), domain.Usage{}))
}

func (gateway *MediaGateway) GenerateVideo(ctx context.Context, model string, request domain.VideoRequest) ([]domain.GeneratedMedia, error) {
	invocation := domain.ModelRequest{
		TenantContext: request.TenantContext, RequestID: request.RequestID, ConversationID: request.ConversationID,
		Prompt: []byte(request.Prompt), MaxOutputTokens: 1,
	}
	lease, err := gateway.acquire(ctx, model, invocation)
	if err != nil {
		return nil, err
	}
	media, callErr := gateway.Provider.GenerateVideo(ctx, model, request)
	return media, errors.Join(callErr, lease.Release(context.WithoutCancel(ctx), domain.Usage{}))
}

func (gateway *MediaGateway) acquire(ctx context.Context, model string, request domain.ModelRequest) (contract.CapacityLease, error) {
	model = strings.TrimSpace(model)
	if gateway == nil || gateway.RateLimiter == nil || gateway.Provider == nil || gateway.Capacity == nil || strings.TrimSpace(gateway.ProviderID) == "" {
		return nil, fmt.Errorf("media gateway is not configured")
	}
	if request.TenantContext.TenantID <= 0 || strings.TrimSpace(request.RequestID) == "" || model == "" {
		return nil, fmt.Errorf("%w: tenant, request id, and model are required", domain.ErrValidation)
	}
	if err := gateway.RateLimiter.AllowModelCall(ctx, request); err != nil {
		return nil, err
	}
	lease, err := gateway.Capacity.Acquire(ctx, request, domain.ModelSelection{
		Provider: gateway.ProviderID, Model: model, Region: gateway.Region, RouteID: gateway.RouteID})
	if err != nil {
		return nil, err
	}
	if lease == nil || strings.TrimSpace(lease.Pool()) == "" {
		invalidErr := fmt.Errorf("%w: capacity scheduler returned an invalid lease", domain.ErrNotReady)
		if lease != nil {
			invalidErr = errors.Join(invalidErr, lease.Release(context.WithoutCancel(ctx), domain.Usage{}))
		}
		return nil, invalidErr
	}
	return lease, nil
}

var _ contract.MediaProvider = (*MediaGateway)(nil)
