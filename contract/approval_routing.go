package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// ApproverResolver is the product's directory; Scout never models roles, positions, or HR.
type ApproverResolver interface {
	// Eligible returns current holders, most preferred first; an empty list is absence, not an error.
	Eligible(ctx context.Context, query domain.ApproverQuery) ([]domain.EligibleApprover, error)
	// Check is domain.ErrNotFound when the tenant has no such role or position.
	Check(ctx context.Context, tenantID int64, reference domain.ApproverReference) error
}

// StudioApprovalRoutingBackend reports an agent's effective approval routing and the asks
// its draft can make.
type StudioApprovalRoutingBackend interface {
	ApprovalRouting(ctx context.Context, actor domain.StudioActor, agentID string) (domain.ApprovalRoutingView, error)
}
