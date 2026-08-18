package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// DelegationGrantRepository stores typed delegation bounds.
type DelegationGrantRepository interface {
	Put(ctx context.Context, grant domain.DelegationGrant) error
	Get(ctx context.Context, tenantID int64, grantID string) (domain.DelegationGrant, error)
	// Revoke ends a grant immediately; in-flight work bound to it must stop.
	Revoke(ctx context.Context, tenantID int64, grantID, reason string) error
	// ForGrantee lists the grants a principal currently holds.
	ForGrantee(ctx context.Context, tenantID int64, grantee domain.PrincipalRef) ([]domain.DelegationGrant, error)
}

// DelegationAuthorizer decides whether one principal may delegate a specific
// action to another, and returns the bounds the delegated principal inherits.
type DelegationAuthorizer interface {
	// Authorize verifies the grant covers the action, is in force, has depth
	// left, and conveys no more than the grantor's own authority.
	Authorize(ctx context.Context, delegator domain.Principal, grantee domain.PrincipalRef, action string) (domain.DelegationAuthorization, error)
}

// AgentInvoker executes one delegated agent call. The composition supplies it:
// in-process for a co-located agent, an A2A client for a remote one. Scout owns
// the authority on the call; the transport is not its concern.
type AgentInvoker interface {
	Invoke(ctx context.Context, call domain.DelegatedCall) (domain.StepResult, error)
}

// WorkItemStore addresses work to a principal rather than to a conversation.
type WorkItemStore interface {
	// Assign records a new item; a repeated request id returns the existing one.
	Assign(ctx context.Context, item domain.WorkItem) (domain.WorkItem, error)
	Get(ctx context.Context, tenantID, id int64) (domain.WorkItem, error)
	// Pending lists open items for one assignee, oldest first.
	Pending(ctx context.Context, tenantID int64, assignee domain.PrincipalRef, limit int) ([]domain.WorkItem, error)
	// Complete records a terminal status for an item.
	Complete(ctx context.Context, tenantID, id int64, status string) error
	// Ancestors returns the item's chain to the root, used for cycle detection.
	Ancestors(ctx context.Context, tenantID, id int64) ([]domain.WorkItem, error)
}
