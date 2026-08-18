package domain

import "time"

// DelegationGrant is the typed bound on who may assign or approve work for whom.
// It may convey only a subset of the grantor's own effective authority, which is
// a set comparison because agents and humans share one authorization model.
type DelegationGrant struct {
	TenantID int64
	GrantID  string
	Grantor  PrincipalRef
	Grantee  PrincipalRef
	// ActionScope is the action pattern the grant covers, matched like a policy action.
	ActionScope string
	// MaxDepth is how many further hops the grantee may delegate; 0 forbids re-delegation.
	MaxDepth         int
	BudgetMinorUnits int64
	Currency         string
	ApprovalRequired bool
	ValidFrom        time.Time
	ValidTo          time.Time
	RevokedAt        time.Time
}

// WorkItem is one unit of work addressed to a principal rather than to a
// conversation, so an agent can be assigned work it did not start.
type WorkItem struct {
	ID        int64
	TenantID  int64
	Assignee  PrincipalRef
	Requester PrincipalRef
	GrantID   string
	ParentID  int64
	// Depth is the delegation depth this item sits at; it only ever increases.
	Depth    int
	ScopeID  string
	TaskKind string
	Input    ObjectRef
	// RequestID links the item to the turn that satisfies it, once dispatched.
	RequestID        string
	Status           string
	BudgetMinorUnits int64
	Currency         string
	CreatedAt        time.Time
	CompletedAt      time.Time
}

// DelegatedCall is one agent invoking another under a verified grant. The
// transport is the composition's choice; the authority on it is not.
type DelegatedCall struct {
	Caller    Principal
	Target    PrincipalRef
	Authority AuthorityHop
	Bounds    DelegationBounds
	WorkItem  WorkItem
	Input     []byte
	RequestID string
}

// DelegationAuthorization is the grant and narrowed authority selected for one hop.
type DelegationAuthorization struct {
	GrantID   string
	Authority AuthorityHop
	Bounds    DelegationBounds
}

// DelegationBounds are the narrowing constraints one hop passes to the next.
// Every field may only shrink as the chain deepens.
type DelegationBounds struct {
	RemainingDepth   int
	BudgetMinorUnits int64
	Currency         string
	ScopeID          string
	ApprovalRequired bool
}
