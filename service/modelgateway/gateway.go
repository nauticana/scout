package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Gateway applies admission and capacity controls around model providers.
type Gateway struct {
	RateLimiter contract.TenantRateLimiter
	Providers   contract.ModelProviderRegistry
	Capacity    contract.CapacityScheduler
}

// NewGateway builds a governed provider entry point.
func NewGateway(rateLimiter contract.TenantRateLimiter, providers contract.ModelProviderRegistry, capacity contract.CapacityScheduler) (*Gateway, error) {
	if rateLimiter == nil || providers == nil || capacity == nil {
		return nil, fmt.Errorf("model gateway: rate limiter, provider registry, and capacity scheduler are required")
	}
	return &Gateway{RateLimiter: rateLimiter, Providers: providers, Capacity: capacity}, nil
}

// Generate performs one governed model invocation and settles its capacity lease.
func (gateway *Gateway) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	if err := gateway.validate(selection, request); err != nil {
		return domain.ModelResult{}, err
	}
	if err := gateway.RateLimiter.AllowModelCall(ctx, request); err != nil {
		return domain.ModelResult{}, err
	}
	provider, err := gateway.Providers.ProviderFor(ctx, selection)
	if err != nil {
		return domain.ModelResult{}, err
	}
	lease, selected, err := gateway.acquire(ctx, selection, request)
	if err != nil {
		return domain.ModelResult{}, err
	}
	result, callErr := provider.Generate(ctx, selected, request)
	releaseErr := lease.Release(context.WithoutCancel(ctx), result.Usage)
	return result, errors.Join(callErr, releaseErr)
}

// Stream performs one governed streaming invocation and returns a lease-owning stream.
func (gateway *Gateway) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	if err := gateway.validate(selection, request); err != nil {
		return nil, err
	}
	if err := gateway.RateLimiter.AllowModelCall(ctx, request); err != nil {
		return nil, err
	}
	provider, err := gateway.Providers.ProviderFor(ctx, selection)
	if err != nil {
		return nil, err
	}
	lease, selected, err := gateway.acquire(ctx, selection, request)
	if err != nil {
		return nil, err
	}
	stream, err := provider.Stream(ctx, selected, request)
	if err != nil {
		return nil, errors.Join(err, lease.Release(context.WithoutCancel(ctx), domain.Usage{}))
	}
	if stream == nil {
		return nil, errors.Join(fmt.Errorf("model provider returned a nil stream"), lease.Release(context.WithoutCancel(ctx), domain.Usage{}))
	}
	return &leasedModelStream{stream: stream, lease: lease, releaseCtx: context.WithoutCancel(ctx)}, nil
}

func (gateway *Gateway) validate(selection domain.ModelSelection, request domain.ModelRequest) error {
	if gateway.RateLimiter == nil || gateway.Providers == nil || gateway.Capacity == nil {
		return fmt.Errorf("model gateway: rate limiter, provider registry, and capacity scheduler are required")
	}
	if request.TenantContext.TenantID <= 0 || strings.TrimSpace(request.RequestID) == "" {
		return fmt.Errorf("%w: tenant and request id are required", domain.ErrValidation)
	}
	if strings.TrimSpace(selection.Provider) == "" || strings.TrimSpace(selection.Model) == "" {
		return fmt.Errorf("%w: model provider and model are required", domain.ErrValidation)
	}
	if request.MaxOutputTokens <= 0 {
		return fmt.Errorf("%w: max output tokens must be positive", domain.ErrValidation)
	}
	return nil
}

func (gateway *Gateway) acquire(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.CapacityLease, domain.ModelSelection, error) {
	lease, err := gateway.Capacity.Acquire(ctx, request, selection)
	if err != nil {
		return nil, domain.ModelSelection{}, err
	}
	if lease == nil || strings.TrimSpace(lease.Pool()) == "" {
		invalidErr := fmt.Errorf("%w: capacity scheduler returned an invalid lease", domain.ErrNotReady)
		if lease != nil {
			invalidErr = errors.Join(invalidErr, lease.Release(context.WithoutCancel(ctx), domain.Usage{}))
		}
		return nil, domain.ModelSelection{}, invalidErr
	}
	selection.CapacityPool = lease.Pool()
	return lease, selection, nil
}

var _ contract.ModelGateway = (*Gateway)(nil)
