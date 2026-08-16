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

// ModelCandidateCatalog is the immutable view of routes a tenant may use.
type ModelCandidateCatalog interface {
	// CandidatesFor returns the tenant's accessible candidates with the catalog generation.
	CandidatesFor(ctx context.Context, tenant domain.TenantContext) (domain.ModelCandidateSet, error)
}

// CapacitySnapshotSource publishes the latest per-route health and queue prediction.
type CapacitySnapshotSource interface {
	// Snapshots returns every known route snapshot; missing routes are unknown, not healthy.
	Snapshots(ctx context.Context) ([]domain.CapacitySnapshot, error)
}

// RouteSnapshotLookup is the optional point-lookup capability of a snapshot source,
// used per stream frame to enforce drain deadlines without copying the whole view.
type RouteSnapshotLookup interface {
	SnapshotFor(ctx context.Context, selection domain.ModelSelection) (domain.CapacitySnapshot, bool, error)
}

// CapacitySnapshotPublisher receives route health from provider or serving adapters.
type CapacitySnapshotPublisher interface {
	Publish(ctx context.Context, snapshot domain.CapacitySnapshot) error
}

// SnapshotSigner authenticates cached routing snapshots so a restored or shared
// snapshot cannot be altered without detection.
type SnapshotSigner interface {
	Sign(payload []byte) []byte
	Verify(payload, signature []byte) bool
}

// TenantRoutingPolicyRepository supplies each tenant's explicit routing preferences.
type TenantRoutingPolicyRepository interface {
	RoutingPolicyFor(ctx context.Context, tenantID int64) (domain.RoutingPolicy, error)
}

// RouteAffinity remembers which route last served an affinity key so the router
// can prefer it; it is a hint store, never an authorization or capacity source.
type RouteAffinity interface {
	// Lookup returns the route last bound to the tenant's affinity key.
	Lookup(ctx context.Context, tenantID int64, affinityKey string) (domain.ModelSelection, bool)
	// Remember binds the affinity key to the selected route.
	Remember(ctx context.Context, tenantID int64, affinityKey string, selection domain.ModelSelection)
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

// ServingSignalObserver receives route-attributed admission and latency samples from
// the gateway and schedulers; a collector aggregates them into ServingSignal windows.
type ServingSignalObserver interface {
	ObserveServing(ctx context.Context, sample domain.ServingSample)
}

// ServingSignalExporter publishes the queued-work and latency signals an external
// serving autoscaler consumes; Scout never places or scales replicas itself.
type ServingSignalExporter interface {
	Export(ctx context.Context, signal domain.ServingSignal) error
}
