package domain

import (
	"encoding/json"
	"time"
)

// AgentState is the operational lifecycle of one agent identity.
type AgentState string

const (
	AgentStateDraft     AgentState = "draft"
	AgentStateActive    AgentState = "active"
	AgentStateSuspended AgentState = "suspended"
	AgentStateDraining  AgentState = "draining"
	AgentStateRetired   AgentState = "retired"
)

// AgentStateChange is one audited transition; every change names a reason and
// the principal that made it.
type AgentStateChange struct {
	TenantID  int64
	AgentID   string
	From      AgentState
	To        AgentState
	Reason    string
	Actor     PrincipalRef
	ChangedAt time.Time
}

// AgentTypeRef pins the type version an instance was created from.
type AgentTypeRef struct {
	AgentTypeID string `json:"agent_type_id"`
	TypeVersion string `json:"type_version"`
}

// AgentType is a reusable behaviour template.
type AgentType struct {
	TenantID    int64
	AgentTypeID string
	DisplayName string
	Description string
	CreatedAt   time.Time
}

// AgentTypeVersion is an immutable published template. Instances pin one and
// never auto-upgrade: the pinned version is the contract.
type AgentTypeVersion struct {
	TenantID    int64           `json:"-"`
	AgentTypeID string          `json:"agent_type_id"`
	TypeVersion string          `json:"type_version"`
	Purpose     string          `json:"purpose,omitempty"`
	Autonomy    AutonomyMode    `json:"autonomy,omitempty"`
	Packages    []CapabilityRef `json:"packages,omitempty"`
	Definition  json.RawMessage `json:"definition,omitempty"`
	Digest      string          `json:"digest"`
	Change      string          `json:"change_summary,omitempty"`
	PublishedBy *int64          `json:"published_by,omitempty"`
	PublishedAt time.Time       `json:"published_at"`
}

// CapabilityRef names one capability package a type version requires.
type CapabilityRef struct {
	PackageID      string `json:"package_id"`
	PackageVersion string `json:"package_version"`
	Required       bool   `json:"required"`
}

// CapabilityPackage is a named, versioned bundle of scoped resource values. It
// is expanded into bindings at instantiation, not a second binding mechanism.
type CapabilityPackage struct {
	TenantID       int64
	PackageID      string
	PackageVersion string
	DisplayName    string
	Resources      []CapabilityResource
	Digest         string
	CreatedAt      time.Time
}

// CapabilityResource is one resource value a package contributes.
type CapabilityResource struct {
	ResourceKind ResourceKind `json:"resource_kind"`
	ResourceID   string       `json:"resource_id"`
	Value        []byte       `json:"value"`
}

// InstantiateRequest creates an agent instance from a published type version.
type InstantiateRequest struct {
	TenantID    int64
	AgentID     string
	DisplayName string
	Type        AgentTypeRef
	ScopeID     string
	// Overlay narrows the type's values at the instance scope; it may never broaden them.
	Overlay   []ScopedBinding
	CreatedBy *int64
}

// ConformanceFinding reports one instance that no longer satisfies its type
// version. A running instance is never auto-upgraded; the finding is the signal.
type ConformanceFinding struct {
	AgentID     string
	PinnedType  AgentTypeRef
	CurrentType AgentTypeRef
	Missing     []CapabilityRef
	Reason      string
}

// AgentQuarantine withdraws one agent version from all traffic. It overrides
// every pin and deployment pointer rather than editing them.
type AgentQuarantine struct {
	TenantID      int64
	AgentID       string
	AgentVersion  string
	Reason        string
	Actor         PrincipalRef
	QuarantinedAt time.Time
	LiftedAt      time.Time
}
