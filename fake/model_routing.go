package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ModelRouterFunc adapts a function to contract.ModelRouter.
type ModelRouterFunc func(context.Context, domain.ModelRequest) (domain.ModelSelection, error)

// Select invokes the configured function.
func (function ModelRouterFunc) Select(ctx context.Context, request domain.ModelRequest) (domain.ModelSelection, error) {
	return function(ctx, request)
}

// ModelGateway contains configurable gateway callbacks.
type ModelGateway struct {
	GenerateFunc func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error)
	StreamFunc   func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error)
}

// Generate invokes GenerateFunc.
func (gateway *ModelGateway) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	return gateway.GenerateFunc(ctx, selection, request)
}

// Stream invokes StreamFunc.
func (gateway *ModelGateway) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	return gateway.StreamFunc(ctx, selection, request)
}

// ModelCandidateCatalog returns a fixed candidate set or error.
type ModelCandidateCatalog struct {
	Set domain.ModelCandidateSet
	Err error
}

// CandidatesFor returns Set unless Err is set.
func (catalog *ModelCandidateCatalog) CandidatesFor(context.Context, domain.TenantContext) (domain.ModelCandidateSet, error) {
	return catalog.Set, catalog.Err
}

// CapacitySnapshotSource returns fixed snapshots or error.
type CapacitySnapshotSource struct {
	Items []domain.CapacitySnapshot
	Err   error
}

// Snapshots returns Items unless Err is set.
func (source *CapacitySnapshotSource) Snapshots(context.Context) ([]domain.CapacitySnapshot, error) {
	return source.Items, source.Err
}

// SnapshotFor finds the route among Items.
func (source *CapacitySnapshotSource) SnapshotFor(_ context.Context, selection domain.ModelSelection) (domain.CapacitySnapshot, bool, error) {
	for _, snapshot := range source.Items {
		if snapshot.Provider == selection.Provider && snapshot.Model == selection.Model && snapshot.Region == selection.Region && snapshot.RouteID == selection.RouteID {
			return snapshot, true, source.Err
		}
	}
	return domain.CapacitySnapshot{}, false, source.Err
}

// TenantRoutingPolicyRepository returns a fixed routing policy or error.
type TenantRoutingPolicyRepository struct {
	Policy domain.RoutingPolicy
	Err    error
}

// RoutingPolicyFor returns Policy unless Err is set.
func (repository *TenantRoutingPolicyRepository) RoutingPolicyFor(context.Context, int64) (domain.RoutingPolicy, error) {
	return repository.Policy, repository.Err
}

// ModelPricerFunc adapts a function to contract.ModelPricer.
type ModelPricerFunc func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error)

// Cost invokes the configured function.
func (function ModelPricerFunc) Cost(ctx context.Context, reference domain.ModelReference, usage domain.ModelUsage) (int64, string, error) {
	return function(ctx, reference, usage)
}

// RouteAffinity is an in-memory affinity hint store.
type RouteAffinity struct {
	Bindings map[string]domain.ModelSelection
}

// Lookup returns the remembered selection for the affinity key.
func (affinity *RouteAffinity) Lookup(_ context.Context, _ int64, key string) (domain.ModelSelection, bool) {
	selection, ok := affinity.Bindings[key]
	return selection, ok
}

// Remember stores the selection for the affinity key.
func (affinity *RouteAffinity) Remember(_ context.Context, _ int64, key string, selection domain.ModelSelection) {
	if affinity.Bindings == nil {
		affinity.Bindings = make(map[string]domain.ModelSelection)
	}
	affinity.Bindings[key] = selection
}

// ServingSignalObserver collects serving samples in memory.
type ServingSignalObserver struct {
	Samples []domain.ServingSample
}

// ObserveServing appends the sample.
func (observer *ServingSignalObserver) ObserveServing(_ context.Context, sample domain.ServingSample) {
	observer.Samples = append(observer.Samples, sample)
}

// ServingSignalExporter collects exported signals in memory.
type ServingSignalExporter struct {
	Signals []domain.ServingSignal
	Err     error
}

// Export appends the signal unless Err is set.
func (exporter *ServingSignalExporter) Export(_ context.Context, signal domain.ServingSignal) error {
	if exporter.Err != nil {
		return exporter.Err
	}
	exporter.Signals = append(exporter.Signals, signal)
	return nil
}

// TenantBudgetManager contains configurable reservation callbacks.
type TenantBudgetManager struct {
	ReserveFunc func(context.Context, int64, string, int64, int64, string) (domain.BudgetReservation, error)
	CommitFunc  func(context.Context, domain.BudgetReservation, domain.Usage) error
	ReleaseFunc func(context.Context, domain.BudgetReservation) error
}

// Reserve invokes ReserveFunc.
func (manager *TenantBudgetManager) Reserve(ctx context.Context, tenantID int64, requestID string, tokens, costMinorUnits int64, currency string) (domain.BudgetReservation, error) {
	return manager.ReserveFunc(ctx, tenantID, requestID, tokens, costMinorUnits, currency)
}

// Commit invokes CommitFunc.
func (manager *TenantBudgetManager) Commit(ctx context.Context, reservation domain.BudgetReservation, usage domain.Usage) error {
	return manager.CommitFunc(ctx, reservation, usage)
}

// Release invokes ReleaseFunc.
func (manager *TenantBudgetManager) Release(ctx context.Context, reservation domain.BudgetReservation) error {
	return manager.ReleaseFunc(ctx, reservation)
}

var _ contract.ModelRouter = ModelRouterFunc(nil)
var _ contract.ModelGateway = (*ModelGateway)(nil)
var _ contract.ModelCandidateCatalog = (*ModelCandidateCatalog)(nil)
var _ contract.CapacitySnapshotSource = (*CapacitySnapshotSource)(nil)
var _ contract.RouteSnapshotLookup = (*CapacitySnapshotSource)(nil)
var _ contract.TenantRoutingPolicyRepository = (*TenantRoutingPolicyRepository)(nil)
var _ contract.ModelPricer = ModelPricerFunc(nil)
var _ contract.RouteAffinity = (*RouteAffinity)(nil)
var _ contract.ServingSignalObserver = (*ServingSignalObserver)(nil)
var _ contract.ServingSignalExporter = (*ServingSignalExporter)(nil)
var _ contract.TenantBudgetManager = (*TenantBudgetManager)(nil)
