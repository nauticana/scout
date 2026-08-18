package domain

import "time"

// PrincipalKind classifies the subject of a governed operation.
type PrincipalKind string

const (
	PrincipalAgent   PrincipalKind = "agent"
	PrincipalHuman   PrincipalKind = "human"
	PrincipalService PrincipalKind = "service"
)

// PrincipalRef names a subject without resolving its authority.
type PrincipalRef struct {
	Kind PrincipalKind `json:"kind"`
	// ID is agent_profile.agent_id for an agent and the decimal user_account.id for a human.
	ID string `json:"id"`
}

// Principal is the acting subject of one governed operation. The zero value is
// never authorized; every enforcement point rejects it.
type Principal struct {
	Kind PrincipalKind
	ID   string
	// TenantID is the owning tenant; a principal never crosses it.
	TenantID int64
	// ScopeID places the principal in the tenant's scope tree; empty means the tenant root.
	ScopeID string
	// Release is the effective release an agent principal is pinned to.
	Release string
	// EntitlementsDigest binds the principal to the entitlement set frozen into Release.
	EntitlementsDigest string
	// Authority is empty when the principal acts on its own behalf.
	Authority AuthorityChain
}

// AuthorityChain is the delegation path in the shape of the RFC 8693 act claim:
// the immediate delegator first, the original authority last.
type AuthorityChain []AuthorityHop

// AuthorityHop is one delegation step. It carries references and bounds, never a credential.
type AuthorityHop struct {
	GrantID string       `json:"grant_id"`
	Grantor PrincipalRef `json:"grantor"`
	// MaxDepth is the remaining delegation depth this hop conveys; 0 forbids further delegation.
	MaxDepth int `json:"max_depth"`
	// BudgetMinorUnits caps spend under this hop; 0 means the grantor's own budget binds.
	BudgetMinorUnits int64  `json:"budget_minor_units"`
	Currency         string `json:"currency,omitempty"`
	ApprovalRequired bool   `json:"approval_required"`
	NotBefore        time.Time
	// NotAfter is the grant's endda; zero means open-ended.
	NotAfter time.Time
}

// AuthorityRef records which authority one governed call exercised, for the audit trail.
type AuthorityRef struct {
	Subject PrincipalRef `json:"subject"`
	GrantID string       `json:"grant_id,omitempty"`
	Grantor PrincipalRef `json:"grantor,omitzero"`
}

// AuthorizationGrant is one resolved keel authorization-object decision.
// LowLimit and HighLimit carry the bounded authority the role conveys.
type AuthorizationGrant struct {
	Allowed     bool
	LowLimit    string
	HighLimit   string
	BypassScope bool
}
