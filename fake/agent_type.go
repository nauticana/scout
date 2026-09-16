package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// AgentTypeRepository contains configurable agent-type reads and publication.
type AgentTypeRepository struct {
	PutTypeFunc   func(context.Context, domain.AgentType) error
	PublishFunc   func(context.Context, domain.AgentTypeVersion) error
	GetFunc       func(context.Context, int64, domain.AgentTypeRef) (domain.AgentTypeVersion, error)
	LatestFunc    func(context.Context, int64, string) (domain.AgentTypeVersion, error)
	InstancesFunc func(context.Context, int64, string) (map[string]domain.AgentTypeRef, error)
}

// PutType invokes PutTypeFunc when configured.
func (r *AgentTypeRepository) PutType(ctx context.Context, agentType domain.AgentType) error {
	if r.PutTypeFunc != nil {
		return r.PutTypeFunc(ctx, agentType)
	}
	return nil
}

// Publish invokes PublishFunc when configured.
func (r *AgentTypeRepository) Publish(ctx context.Context, version domain.AgentTypeVersion) error {
	if r.PublishFunc != nil {
		return r.PublishFunc(ctx, version)
	}
	return nil
}

// Get invokes GetFunc when configured.
func (r *AgentTypeRepository) Get(ctx context.Context, tenantID int64, ref domain.AgentTypeRef) (domain.AgentTypeVersion, error) {
	if r.GetFunc != nil {
		return r.GetFunc(ctx, tenantID, ref)
	}
	return domain.AgentTypeVersion{}, domain.ErrNotFound
}

// Latest invokes LatestFunc when configured.
func (r *AgentTypeRepository) Latest(ctx context.Context, tenantID int64, agentTypeID string) (domain.AgentTypeVersion, error) {
	if r.LatestFunc != nil {
		return r.LatestFunc(ctx, tenantID, agentTypeID)
	}
	return domain.AgentTypeVersion{}, domain.ErrNotFound
}

// Instances invokes InstancesFunc when configured.
func (r *AgentTypeRepository) Instances(ctx context.Context, tenantID int64, agentTypeID string) (map[string]domain.AgentTypeRef, error) {
	if r.InstancesFunc != nil {
		return r.InstancesFunc(ctx, tenantID, agentTypeID)
	}
	return nil, nil
}

// CapabilityPackageRepository contains configurable package reads and writes.
type CapabilityPackageRepository struct {
	PutPackageFunc func(context.Context, domain.CapabilityPackage) error
	GetPackageFunc func(context.Context, int64, string, string) (domain.CapabilityPackage, error)
}

// PutPackage invokes PutPackageFunc when configured.
func (r *CapabilityPackageRepository) PutPackage(ctx context.Context, pkg domain.CapabilityPackage) error {
	if r.PutPackageFunc != nil {
		return r.PutPackageFunc(ctx, pkg)
	}
	return nil
}

// GetPackage invokes GetPackageFunc when configured.
func (r *CapabilityPackageRepository) GetPackage(ctx context.Context, tenantID int64, packageID, packageVersion string) (domain.CapabilityPackage, error) {
	if r.GetPackageFunc != nil {
		return r.GetPackageFunc(ctx, tenantID, packageID, packageVersion)
	}
	return domain.CapabilityPackage{}, domain.ErrNotFound
}

// AgentTypeService contains configurable instantiation and conformance.
type AgentTypeService struct {
	InstantiateFunc func(context.Context, domain.InstantiateRequest) (domain.AgentTypeRef, error)
	ConformanceFunc func(context.Context, int64, string) ([]domain.ConformanceFinding, error)
}

// Instantiate invokes InstantiateFunc when configured.
func (s *AgentTypeService) Instantiate(ctx context.Context, request domain.InstantiateRequest) (domain.AgentTypeRef, error) {
	if s.InstantiateFunc != nil {
		return s.InstantiateFunc(ctx, request)
	}
	return request.Type, nil
}

// Conformance invokes ConformanceFunc when configured.
func (s *AgentTypeService) Conformance(ctx context.Context, tenantID int64, agentTypeID string) ([]domain.ConformanceFinding, error) {
	if s.ConformanceFunc != nil {
		return s.ConformanceFunc(ctx, tenantID, agentTypeID)
	}
	return nil, nil
}

var (
	_ contract.AgentTypeRepository         = (*AgentTypeRepository)(nil)
	_ contract.CapabilityPackageRepository = (*CapabilityPackageRepository)(nil)
	_ contract.AgentTypeService            = (*AgentTypeService)(nil)
)
