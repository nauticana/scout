package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Gateway applies admission and capacity controls around model providers.
type Gateway struct {
	RateLimiter contract.TenantRateLimiter
	Providers   contract.ModelProviderRegistry
	Capacity    contract.CapacityScheduler
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

type leasedModelStream struct {
	receiveMu  sync.Mutex
	mu         sync.Mutex
	stream     contract.ModelStream
	lease      contract.CapacityLease
	releaseCtx context.Context
	usage      domain.Usage
	closed     bool
}

func (stream *leasedModelStream) Receive(ctx context.Context) (domain.ModelChunk, error) {
	stream.receiveMu.Lock()
	defer stream.receiveMu.Unlock()
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return domain.ModelChunk{}, io.EOF
	}
	stream.mu.Unlock()
	chunk, receiveErr := stream.stream.Receive(ctx)
	usageErr := stream.addUsage(chunk.Usage)
	if receiveErr != nil || usageErr != nil || chunk.FinishReason != "" {
		finishErr := stream.finish(ctx)
		return chunk, errors.Join(receiveErr, usageErr, finishErr)
	}
	return chunk, nil
}

func (stream *leasedModelStream) Close() error {
	stream.receiveMu.Lock()
	defer stream.receiveMu.Unlock()
	return stream.finish(stream.releaseCtx)
}

func (stream *leasedModelStream) addUsage(usage domain.Usage) error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.CostMinorUnits < 0 {
		return fmt.Errorf("%w: model stream usage cannot be negative", domain.ErrValidation)
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if usage.Currency != "" && stream.usage.Currency != "" && usage.Currency != stream.usage.Currency {
		return fmt.Errorf("%w: model stream usage currencies differ", domain.ErrValidation)
	}
	stream.usage.InputTokens += usage.InputTokens
	stream.usage.OutputTokens += usage.OutputTokens
	stream.usage.ToolCalls += usage.ToolCalls
	stream.usage.CostMinorUnits += usage.CostMinorUnits
	if stream.usage.Currency == "" {
		stream.usage.Currency = usage.Currency
	}
	return nil
}

func (stream *leasedModelStream) finish(ctx context.Context) error {
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return nil
	}
	stream.closed = true
	usage := stream.usage
	stream.mu.Unlock()
	return errors.Join(stream.stream.Close(), stream.lease.Release(context.WithoutCancel(ctx), usage))
}

var _ contract.ModelGateway = (*Gateway)(nil)
var _ contract.ModelStream = (*leasedModelStream)(nil)
