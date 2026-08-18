package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// AgentTypeRepository stores publishable behaviour templates.
type AgentTypeRepository interface {
	PutType(ctx context.Context, agentType domain.AgentType) error
	Publish(ctx context.Context, version domain.AgentTypeVersion) error
	Get(ctx context.Context, tenantID int64, ref domain.AgentTypeRef) (domain.AgentTypeVersion, error)
	// Latest returns the newest published version of a type.
	Latest(ctx context.Context, tenantID int64, agentTypeID string) (domain.AgentTypeVersion, error)
	// Instances lists the agents pinned to a type, with the version each pins.
	Instances(ctx context.Context, tenantID int64, agentTypeID string) (map[string]domain.AgentTypeRef, error)
}

// CapabilityPackageRepository stores the reusable bundles a type version requires.
type CapabilityPackageRepository interface {
	PutPackage(ctx context.Context, pkg domain.CapabilityPackage) error
	GetPackage(ctx context.Context, tenantID int64, packageID, packageVersion string) (domain.CapabilityPackage, error)
}

// AgentTypeService creates instances from a type version and reports drift.
type AgentTypeService interface {
	// Instantiate creates an agent pinned to a type version, expanding its
	// capability packages into scope bindings. The overlay is narrowing-checked
	// at creation, so an instance can never be born broader than its type.
	Instantiate(ctx context.Context, request domain.InstantiateRequest) (domain.AgentTypeRef, error)
	// Conformance reports instances that no longer satisfy the type's latest
	// version. It never upgrades one: the pinned version is the contract.
	Conformance(ctx context.Context, tenantID int64, agentTypeID string) ([]domain.ConformanceFinding, error)
}

// AgentLifecycle owns the audited state machine of one agent identity.
type AgentLifecycle interface {
	// Transition moves an agent between states, recording reason and actor. An
	// illegal transition is domain.ErrConflict.
	Transition(ctx context.Context, change domain.AgentStateChange) error
	State(ctx context.Context, tenantID int64, agentID string) (domain.AgentState, error)
}

// AgentVersionQuarantine withdraws one agent version from all traffic without
// editing the deployment pointers it would otherwise be selected by.
type AgentVersionQuarantine interface {
	Quarantine(ctx context.Context, quarantine domain.AgentQuarantine) error
	Lift(ctx context.Context, quarantine domain.AgentQuarantine) error
	// Quarantined reports whether a version is withdrawn; the runtime checks it
	// after resolving a version and before serving it.
	Quarantined(ctx context.Context, tenantID int64, agentID, agentVersion string) (bool, error)
}
