package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// TenantRateLimiter contains configurable admission callbacks.
type TenantRateLimiter struct {
	AllowTurnFunc      func(context.Context, domain.TenantContext) error
	AllowToolCallFunc  func(context.Context, domain.ToolCall) error
	AllowModelCallFunc func(context.Context, domain.ModelRequest) error
}

// AllowTurn invokes AllowTurnFunc when configured.
func (limiter *TenantRateLimiter) AllowTurn(ctx context.Context, tenant domain.TenantContext) error {
	if limiter.AllowTurnFunc == nil {
		return nil
	}
	return limiter.AllowTurnFunc(ctx, tenant)
}

// AllowToolCall invokes AllowToolCallFunc when configured.
func (limiter *TenantRateLimiter) AllowToolCall(ctx context.Context, call domain.ToolCall) error {
	if limiter.AllowToolCallFunc == nil {
		return nil
	}
	return limiter.AllowToolCallFunc(ctx, call)
}

// AllowModelCall invokes AllowModelCallFunc when configured.
func (limiter *TenantRateLimiter) AllowModelCall(ctx context.Context, request domain.ModelRequest) error {
	if limiter.AllowModelCallFunc == nil {
		return nil
	}
	return limiter.AllowModelCallFunc(ctx, request)
}

// ModelProvider contains configurable model provider callbacks.
type ModelProvider struct {
	GenerateFunc func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error)
	StreamFunc   func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error)
}

// Generate invokes GenerateFunc.
func (provider *ModelProvider) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	return provider.GenerateFunc(ctx, selection, request)
}

// Stream invokes StreamFunc.
func (provider *ModelProvider) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	return provider.StreamFunc(ctx, selection, request)
}

// ModelStream contains configurable stream callbacks.
type ModelStream struct {
	ReceiveFunc func(context.Context) (domain.ModelChunk, error)
	CloseFunc   func() error
}

// Receive invokes ReceiveFunc.
func (stream *ModelStream) Receive(ctx context.Context) (domain.ModelChunk, error) {
	return stream.ReceiveFunc(ctx)
}

// Close invokes CloseFunc.
func (stream *ModelStream) Close() error {
	return stream.CloseFunc()
}

// CapacitySchedulerFunc adapts a function to contract.CapacityScheduler.
type CapacitySchedulerFunc func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error)

// Acquire invokes the configured function.
func (function CapacitySchedulerFunc) Acquire(ctx context.Context, request domain.ModelRequest, selection domain.ModelSelection) (contract.CapacityLease, error) {
	return function(ctx, request, selection)
}

// CapacityLease contains configurable capacity lease behavior.
type CapacityLease struct {
	PoolValue   string
	ReleaseFunc func(context.Context, domain.Usage) error
}

// Pool returns PoolValue.
func (lease *CapacityLease) Pool() string {
	return lease.PoolValue
}

// Release invokes ReleaseFunc.
func (lease *CapacityLease) Release(ctx context.Context, usage domain.Usage) error {
	return lease.ReleaseFunc(ctx, usage)
}

var _ contract.TenantRateLimiter = (*TenantRateLimiter)(nil)
var _ contract.ModelProvider = (*ModelProvider)(nil)
var _ contract.ModelStream = (*ModelStream)(nil)
var _ contract.CapacityScheduler = CapacitySchedulerFunc(nil)
var _ contract.CapacityLease = (*CapacityLease)(nil)
