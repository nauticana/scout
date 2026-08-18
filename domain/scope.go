package domain

import "time"

// ResourceKind names a kind of configuration a scope may bind. Each kind has one
// registered merger and one narrowing comparator.
type ResourceKind string

const (
	ResourcePromptSection ResourceKind = "prompt_section"
	ResourcePolicy        ResourceKind = "policy"
	ResourceTool          ResourceKind = "tool"
	ResourceKnowledge     ResourceKind = "knowledge"
	ResourceModel         ResourceKind = "model"
	ResourceEntitlement   ResourceKind = "entitlement"
	ResourceBudget        ResourceKind = "budget"
	ResourceAutonomy      ResourceKind = "autonomy"
)

// MergeMode is how a child binding combines with the value it inherits.
type MergeMode string

const (
	MergeReplace   MergeMode = "replace"
	MergeAppend    MergeMode = "append"
	MergeIntersect MergeMode = "intersect"
)

// AutonomyMode is the ordered operating mode a principal may act under; a child
// scope may lower it and never raise it.
type AutonomyMode string

const (
	AutonomyHumanOnly           AutonomyMode = "human_only"
	AutonomyAdvise              AutonomyMode = "advise"
	AutonomyDraft               AutonomyMode = "draft"
	AutonomyExecuteWithApproval AutonomyMode = "execute_with_approval"
	AutonomyBounded             AutonomyMode = "bounded_autonomous"
)

// Scope is one node of a tenant's configuration hierarchy. ScopeKind is opaque to
// Scout: the product names its own levels.
type Scope struct {
	TenantID      int64
	ScopeID       string
	ParentScopeID string
	ScopeKind     string
	DisplayName   string
}

// ScopeChain is a resolved ancestry ordered widest first, so a later element
// may only narrow an earlier one.
type ScopeChain []Scope

// ScopedBinding attaches one versioned resource value to a scope. Sealed is set
// by the binding's own scope and forbids any narrower scope overriding it.
type ScopedBinding struct {
	TenantID        int64
	ScopeID         string
	ResourceKind    ResourceKind
	ResourceID      string
	ResourceVersion string
	MergeMode       MergeMode
	Sealed          bool
	Value           []byte
	ValueDigest     string
	ValidFrom       time.Time
	ValidTo         time.Time
	BoundBy         *int64
}

// Provenance records why one effective resource holds its value.
type Provenance struct {
	ScopeID         string       `json:"scope_id"`
	ScopeKind       string       `json:"scope_kind"`
	ResourceKind    ResourceKind `json:"resource_kind"`
	ResourceID      string       `json:"resource_id"`
	ResourceVersion string       `json:"resource_version"`
	MergeMode       MergeMode    `json:"merge_mode"`
	Sealed          bool         `json:"sealed"`
	Approver        *int64       `json:"approver,omitempty"`
	CompiledAt      time.Time    `json:"compiled_at"`
}

// EffectiveResource is one compiled resource with the provenance of the binding
// that won and of every binding it superseded, so an explain view never recompiles.
type EffectiveResource struct {
	ResourceKind ResourceKind `json:"resource_kind"`
	ResourceID   string       `json:"resource_id"`
	Value        []byte       `json:"value"`
	Source       Provenance   `json:"source"`
	Superseded   []Provenance `json:"superseded,omitempty"`
}

// EffectiveRelease is the immutable configuration a conversation pins so an edit
// cannot change work already in progress.
type EffectiveRelease struct {
	TenantID     int64               `json:"tenant_id"`
	AgentID      string              `json:"agent_id"`
	AgentVersion string              `json:"agent_version"`
	ScopeID      string              `json:"scope_id"`
	Resources    []EffectiveResource `json:"resources"`
	Digest       string              `json:"digest"`
	CompiledBy   *int64              `json:"compiled_by,omitempty"`
	CompiledAt   time.Time           `json:"compiled_at"`
}

// ResourceExplanation is one effective value with the binding that won and the
// bindings it beat, ordered widest scope first.
type ResourceExplanation struct {
	ResourceKind ResourceKind
	ResourceID   string
	Value        []byte
	Winner       Provenance
	Superseded   []Provenance
	// Sealed reports that no narrower scope may override this value.
	Sealed bool
}

// ResourceChange classifies one entry in a release diff.
type ResourceChange string

const (
	ResourceAdded    ResourceChange = "added"
	ResourceRemoved  ResourceChange = "removed"
	ResourceModified ResourceChange = "modified"
)

// ResourceDiff is one resource that differs between two compiled releases.
type ResourceDiff struct {
	ResourceKind ResourceKind
	ResourceID   string
	Change       ResourceChange
	From         []byte
	To           []byte
	FromSource   Provenance
	ToSource     Provenance
}

// CompileRequest asks for the effective configuration of one agent version.
type CompileRequest struct {
	TenantID     int64
	AgentID      string
	AgentVersion string
	ScopeID      string
	CompiledBy   *int64
	// AsOf selects the bindings in force at a point in time; zero means now.
	AsOf time.Time
}
