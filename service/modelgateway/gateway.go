package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Capacity outcome labels the gateway reports as serving samples.
const (
	CapacityOutcomeGranted   = "granted"
	CapacityOutcomeRejected  = "rejected"
	CapacityOutcomeCompleted = "completed"
	CapacityOutcomeFailed    = "failed"
	CapacityOutcomeCanceled  = "canceled"
)

// Gateway applies admission and capacity controls around model providers and
// carries the full route identity of the selection through every call.
type Gateway struct {
	RateLimiter contract.TenantRateLimiter
	Providers   contract.ModelProviderRegistry
	Capacity    contract.CapacityScheduler
	// Observer receives one StageModel observation per call when set.
	Observer contract.ObservationRecorder
	// Signals receives admission and completion samples per route when set.
	Signals contract.ServingSignalObserver
	// PromptTokens estimates prompt work for serving samples; nil uses EstimatePromptTokens.
	PromptTokens func([]byte) int64
	Now          func() time.Time
}

// NewGateway builds a governed provider entry point.
func NewGateway(rateLimiter contract.TenantRateLimiter, providers contract.ModelProviderRegistry, capacity contract.CapacityScheduler) (*Gateway, error) {
	if rateLimiter == nil || providers == nil || capacity == nil {
		return nil, fmt.Errorf("model gateway: rate limiter, provider registry, and capacity scheduler are required")
	}
	return &Gateway{RateLimiter: rateLimiter, Providers: providers, Capacity: capacity}, nil
}

func (gateway *Gateway) now() time.Time {
	if gateway.Now == nil {
		return time.Now()
	}
	return gateway.Now()
}

// Generate performs one governed model invocation and settles its capacity lease.
func (gateway *Gateway) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	call, err := gateway.admit(ctx, selection, request)
	if err != nil {
		return domain.ModelResult{}, err
	}
	provider, err := gateway.Providers.ProviderFor(ctx, selection)
	if err != nil {
		call.reject(ctx, err)
		return domain.ModelResult{}, err
	}
	lease, selected, err := gateway.acquire(ctx, call, selection, request)
	if err != nil {
		return domain.ModelResult{}, err
	}
	result, callErr := provider.Generate(ctx, selected, request)
	releaseErr := lease.Release(context.WithoutCancel(ctx), result.Usage)
	call.finish(context.WithoutCancel(ctx), result.Usage, callErr)
	return result, errors.Join(callErr, releaseErr)
}

// Stream performs one governed streaming invocation and returns a lease-owning stream.
func (gateway *Gateway) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	call, err := gateway.admit(ctx, selection, request)
	if err != nil {
		return nil, err
	}
	provider, err := gateway.Providers.ProviderFor(ctx, selection)
	if err != nil {
		call.reject(ctx, err)
		return nil, err
	}
	lease, selected, err := gateway.acquire(ctx, call, selection, request)
	if err != nil {
		return nil, err
	}
	stream, err := provider.Stream(ctx, selected, request)
	if err == nil && stream == nil {
		err = fmt.Errorf("model provider returned a nil stream")
	}
	if err != nil {
		call.finish(context.WithoutCancel(ctx), domain.Usage{}, err)
		return nil, errors.Join(err, lease.Release(context.WithoutCancel(ctx), domain.Usage{}))
	}
	return &leasedModelStream{stream: stream, lease: lease, call: call, releaseCtx: context.WithoutCancel(ctx)}, nil
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

// admit validates and rate-limits; a rejection is observed as an admission rejection.
func (gateway *Gateway) admit(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (*modelCall, error) {
	if err := gateway.validate(selection, request); err != nil {
		return nil, err
	}
	call := &modelCall{gateway: gateway, selection: selection, request: request, started: gateway.now(),
		prefillTokens: promptTokens(gateway.PromptTokens, request.Prompt)}
	if err := gateway.RateLimiter.AllowModelCall(ctx, request); err != nil {
		call.admissionRejected(ctx, err)
		return nil, err
	}
	return call, nil
}

// acquire binds capacity to the selection, preserving every routing field and stamping the pool.
func (gateway *Gateway) acquire(ctx context.Context, call *modelCall, selection domain.ModelSelection, request domain.ModelRequest) (contract.CapacityLease, domain.ModelSelection, error) {
	lease, err := gateway.Capacity.Acquire(ctx, request, selection)
	if err != nil {
		call.reject(ctx, err)
		return nil, domain.ModelSelection{}, err
	}
	if lease == nil || strings.TrimSpace(lease.Pool()) == "" {
		invalidErr := fmt.Errorf("%w: capacity scheduler returned an invalid lease", domain.ErrNotReady)
		if lease != nil {
			invalidErr = errors.Join(invalidErr, lease.Release(context.WithoutCancel(ctx), domain.Usage{}))
		}
		call.reject(ctx, invalidErr)
		return nil, domain.ModelSelection{}, invalidErr
	}
	selection.CapacityPool = lease.Pool()
	call.granted(ctx, selection)
	return lease, selection, nil
}

var _ contract.ModelGateway = (*Gateway)(nil)
