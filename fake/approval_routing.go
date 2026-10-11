package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ApproverResolver is a configurable product directory; the defaults find nobody and know every reference.
type ApproverResolver struct {
	EligibleFunc func(context.Context, domain.ApproverQuery) ([]domain.EligibleApprover, error)
	CheckFunc    func(context.Context, int64, domain.ApproverReference) error
}

// Eligible invokes EligibleFunc when configured.
func (r *ApproverResolver) Eligible(ctx context.Context, query domain.ApproverQuery) ([]domain.EligibleApprover, error) {
	if r.EligibleFunc != nil {
		return r.EligibleFunc(ctx, query)
	}
	return nil, nil
}

// Check invokes CheckFunc when configured.
func (r *ApproverResolver) Check(ctx context.Context, tenantID int64, reference domain.ApproverReference) error {
	if r.CheckFunc != nil {
		return r.CheckFunc(ctx, tenantID, reference)
	}
	return nil
}

// ApproverRouterFunc adapts a function to contract.ApproverRouter.
type ApproverRouterFunc func(context.Context, domain.ApprovalRequest, domain.PrincipalRef) (domain.ApprovalAssignment, error)

// Route invokes the configured function.
func (function ApproverRouterFunc) Route(ctx context.Context, request domain.ApprovalRequest, prefer domain.PrincipalRef) (domain.ApprovalAssignment, error) {
	return function(ctx, request, prefer)
}

// StudioApprovalRoutingBackendFunc adapts a function to contract.StudioApprovalRoutingBackend.
type StudioApprovalRoutingBackendFunc func(context.Context, domain.StudioActor, string) (domain.ApprovalRoutingView, error)

// ApprovalRouting invokes the configured function.
func (function StudioApprovalRoutingBackendFunc) ApprovalRouting(ctx context.Context, actor domain.StudioActor, agentID string) (domain.ApprovalRoutingView, error) {
	return function(ctx, actor, agentID)
}

var (
	_ contract.ApproverResolver             = (*ApproverResolver)(nil)
	_ contract.ApproverRouter               = ApproverRouterFunc(nil)
	_ contract.StudioApprovalRoutingBackend = StudioApprovalRoutingBackendFunc(nil)
)
