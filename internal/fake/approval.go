package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ApprovalStore contains configurable approval reads and writes.
type ApprovalStore struct {
	OpenFunc    func(context.Context, domain.ApprovalRequest) (domain.ApprovalRequest, error)
	GetFunc     func(context.Context, domain.ApprovalKey) (domain.ApprovalRequest, error)
	ResolveFunc func(context.Context, domain.ApprovalVerdict) (domain.ApprovalRequest, error)
}

// Open invokes OpenFunc when configured; the default returns the request pending.
func (s *ApprovalStore) Open(ctx context.Context, request domain.ApprovalRequest) (domain.ApprovalRequest, error) {
	if s.OpenFunc != nil {
		return s.OpenFunc(ctx, request)
	}
	request.Status = domain.ApprovalStatusPending
	return request, nil
}

// Get invokes GetFunc when configured.
func (s *ApprovalStore) Get(ctx context.Context, key domain.ApprovalKey) (domain.ApprovalRequest, error) {
	if s.GetFunc != nil {
		return s.GetFunc(ctx, key)
	}
	return domain.ApprovalRequest{}, domain.ErrNotFound
}

// Resolve invokes ResolveFunc when configured.
func (s *ApprovalStore) Resolve(ctx context.Context, verdict domain.ApprovalVerdict) (domain.ApprovalRequest, error) {
	if s.ResolveFunc != nil {
		return s.ResolveFunc(ctx, verdict)
	}
	return domain.ApprovalRequest{Status: verdict.Status}, nil
}

// ApprovalInbox contains configurable reviewer-facing reads.
type ApprovalInbox struct {
	PendingFunc func(context.Context, domain.ApprovalFilter) ([]domain.ApprovalRequest, error)
	DueByFunc   func(context.Context, int64, domain.ApprovalFilter) ([]domain.ApprovalRequest, error)
}

// Pending invokes PendingFunc when configured.
func (i *ApprovalInbox) Pending(ctx context.Context, filter domain.ApprovalFilter) ([]domain.ApprovalRequest, error) {
	if i.PendingFunc != nil {
		return i.PendingFunc(ctx, filter)
	}
	return nil, nil
}

// DueBy invokes DueByFunc when configured.
func (i *ApprovalInbox) DueBy(ctx context.Context, tenantID int64, filter domain.ApprovalFilter) ([]domain.ApprovalRequest, error) {
	if i.DueByFunc != nil {
		return i.DueByFunc(ctx, tenantID, filter)
	}
	return nil, nil
}

// EscalationPolicyFunc adapts a function to contract.EscalationPolicy.
type EscalationPolicyFunc func(context.Context, domain.ApprovalRequest) (domain.EscalationStep, error)

// Escalate invokes the configured function.
func (function EscalationPolicyFunc) Escalate(ctx context.Context, request domain.ApprovalRequest) (domain.EscalationStep, error) {
	return function(ctx, request)
}

var (
	_ contract.ApprovalStore    = (*ApprovalStore)(nil)
	_ contract.ApprovalInbox    = (*ApprovalInbox)(nil)
	_ contract.EscalationPolicy = EscalationPolicyFunc(nil)
)
