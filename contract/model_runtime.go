package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// ModelRouter selects models and capacity based on request complexity.
type ModelRouter interface {
	// Select chooses a model and capacity pool from complexity and tenant policy.
	Select(ctx context.Context, request domain.ModelRequest) (domain.ModelSelection, error)
}

// ModelGateway is the governed entry point for model inference.
type ModelGateway interface {
	// Generate executes inference through the selected governed provider.
	Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error)
	// Stream executes inference and returns ordered model response frames.
	Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (ModelStream, error)
}

// ModelProvider adapts a specific inference provider.
type ModelProvider interface {
	// Generate performs one bounded inference request for a selected model.
	Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error)
	// Stream performs one bounded streaming request for a selected model.
	Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (ModelStream, error)
}

// ModelStream receives ordered frames from one model invocation.
type ModelStream interface {
	// Receive waits for the next model response frame.
	Receive(ctx context.Context) (domain.ModelChunk, error)
	// Close cancels the provider stream and releases its resources.
	Close() error
}

// ModelProviderRegistry resolves configured inference provider adapters.
type ModelProviderRegistry interface {
	// ProviderFor returns the configured provider adapter for a model selection.
	ProviderFor(ctx context.Context, selection domain.ModelSelection) (ModelProvider, error)
}

// CapacityScheduler allocates shared or dedicated inference capacity.
type CapacityScheduler interface {
	// Acquire reserves shared or dedicated inference capacity by priority class.
	Acquire(ctx context.Context, request domain.ModelRequest, selection domain.ModelSelection) (CapacityLease, error)
}

// CapacityLease represents reserved inference capacity.
type CapacityLease interface {
	// Pool returns the capacity pool granted to the request.
	Pool() string
	// Release returns unused capacity and records actual usage.
	Release(ctx context.Context, usage domain.Usage) error
}
