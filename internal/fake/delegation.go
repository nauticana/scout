package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DelegationAuthorizerFunc adapts a function to contract.DelegationAuthorizer.
type DelegationAuthorizerFunc func(context.Context, domain.Principal, domain.PrincipalRef, string) (domain.DelegationAuthorization, error)

// Authorize invokes the configured function.
func (function DelegationAuthorizerFunc) Authorize(ctx context.Context, delegator domain.Principal, grantee domain.PrincipalRef, action string) (domain.DelegationAuthorization, error) {
	return function(ctx, delegator, grantee, action)
}

// AgentInvokerFunc adapts a function to contract.AgentInvoker.
type AgentInvokerFunc func(context.Context, domain.DelegatedCall) (domain.StepResult, error)

// Invoke invokes the configured function.
func (function AgentInvokerFunc) Invoke(ctx context.Context, call domain.DelegatedCall) (domain.StepResult, error) {
	return function(ctx, call)
}

// EntitlementResolverFunc adapts a function to contract.EntitlementResolver.
type EntitlementResolverFunc func(context.Context, domain.Principal) ([]byte, string, error)

// Entitlements invokes the configured function.
func (function EntitlementResolverFunc) Entitlements(ctx context.Context, principal domain.Principal) ([]byte, string, error) {
	return function(ctx, principal)
}

// AgentLifecycle contains configurable state-machine callbacks.
type AgentLifecycle struct {
	TransitionFunc func(context.Context, domain.AgentStateChange) error
	StateFunc      func(context.Context, int64, string) (domain.AgentState, error)
}

// Transition invokes TransitionFunc when configured.
func (l *AgentLifecycle) Transition(ctx context.Context, change domain.AgentStateChange) error {
	if l.TransitionFunc != nil {
		return l.TransitionFunc(ctx, change)
	}
	return nil
}

// State invokes StateFunc when configured; the default is active.
func (l *AgentLifecycle) State(ctx context.Context, tenantID int64, agentID string) (domain.AgentState, error) {
	if l.StateFunc != nil {
		return l.StateFunc(ctx, tenantID, agentID)
	}
	return domain.AgentStateActive, nil
}

// AgentVersionQuarantine contains configurable quarantine callbacks.
type AgentVersionQuarantine struct {
	QuarantineFunc  func(context.Context, domain.AgentQuarantine) error
	LiftFunc        func(context.Context, domain.AgentQuarantine) error
	QuarantinedFunc func(context.Context, int64, string, string) (bool, error)
}

// Quarantine invokes QuarantineFunc when configured.
func (q *AgentVersionQuarantine) Quarantine(ctx context.Context, quarantine domain.AgentQuarantine) error {
	if q.QuarantineFunc != nil {
		return q.QuarantineFunc(ctx, quarantine)
	}
	return nil
}

// Lift invokes LiftFunc when configured.
func (q *AgentVersionQuarantine) Lift(ctx context.Context, quarantine domain.AgentQuarantine) error {
	if q.LiftFunc != nil {
		return q.LiftFunc(ctx, quarantine)
	}
	return nil
}

// Quarantined invokes QuarantinedFunc when configured.
func (q *AgentVersionQuarantine) Quarantined(ctx context.Context, tenantID int64, agentID, agentVersion string) (bool, error) {
	if q.QuarantinedFunc != nil {
		return q.QuarantinedFunc(ctx, tenantID, agentID, agentVersion)
	}
	return false, nil
}

var (
	_ contract.DelegationAuthorizer   = DelegationAuthorizerFunc(nil)
	_ contract.AgentInvoker           = AgentInvokerFunc(nil)
	_ contract.EntitlementResolver    = EntitlementResolverFunc(nil)
	_ contract.AgentLifecycle         = (*AgentLifecycle)(nil)
	_ contract.AgentVersionQuarantine = (*AgentVersionQuarantine)(nil)
)

// DelegationGrantRepository contains configurable grant storage.
type DelegationGrantRepository struct {
	PutFunc        func(context.Context, domain.DelegationGrant) error
	GetFunc        func(context.Context, int64, string) (domain.DelegationGrant, error)
	RevokeFunc     func(context.Context, int64, string, string) error
	ForGranteeFunc func(context.Context, int64, domain.PrincipalRef) ([]domain.DelegationGrant, error)
}

// Put invokes PutFunc when configured.
func (r *DelegationGrantRepository) Put(ctx context.Context, grant domain.DelegationGrant) error {
	if r.PutFunc != nil {
		return r.PutFunc(ctx, grant)
	}
	return nil
}

// Get invokes GetFunc when configured.
func (r *DelegationGrantRepository) Get(ctx context.Context, tenantID int64, grantID string) (domain.DelegationGrant, error) {
	if r.GetFunc != nil {
		return r.GetFunc(ctx, tenantID, grantID)
	}
	return domain.DelegationGrant{}, domain.ErrNotFound
}

// Revoke invokes RevokeFunc when configured.
func (r *DelegationGrantRepository) Revoke(ctx context.Context, tenantID int64, grantID, reason string) error {
	if r.RevokeFunc != nil {
		return r.RevokeFunc(ctx, tenantID, grantID, reason)
	}
	return nil
}

// ForGrantee invokes ForGranteeFunc when configured.
func (r *DelegationGrantRepository) ForGrantee(ctx context.Context, tenantID int64, grantee domain.PrincipalRef) ([]domain.DelegationGrant, error) {
	if r.ForGranteeFunc != nil {
		return r.ForGranteeFunc(ctx, tenantID, grantee)
	}
	return nil, nil
}

// WorkItemStore contains configurable principal-addressed work storage.
type WorkItemStore struct {
	AssignFunc    func(context.Context, domain.WorkItem) (domain.WorkItem, error)
	GetFunc       func(context.Context, int64, int64) (domain.WorkItem, error)
	PendingFunc   func(context.Context, int64, domain.PrincipalRef, int) ([]domain.WorkItem, error)
	CompleteFunc  func(context.Context, int64, int64, string) error
	AncestorsFunc func(context.Context, int64, int64) ([]domain.WorkItem, error)
}

// Assign invokes AssignFunc when configured.
func (s *WorkItemStore) Assign(ctx context.Context, item domain.WorkItem) (domain.WorkItem, error) {
	if s.AssignFunc != nil {
		return s.AssignFunc(ctx, item)
	}
	item.ID, item.Status = 1, "queued"
	return item, nil
}

// Get invokes GetFunc when configured.
func (s *WorkItemStore) Get(ctx context.Context, tenantID, id int64) (domain.WorkItem, error) {
	if s.GetFunc != nil {
		return s.GetFunc(ctx, tenantID, id)
	}
	return domain.WorkItem{}, domain.ErrNotFound
}

// Pending invokes PendingFunc when configured.
func (s *WorkItemStore) Pending(ctx context.Context, tenantID int64, assignee domain.PrincipalRef, limit int) ([]domain.WorkItem, error) {
	if s.PendingFunc != nil {
		return s.PendingFunc(ctx, tenantID, assignee, limit)
	}
	return nil, nil
}

// Complete invokes CompleteFunc when configured.
func (s *WorkItemStore) Complete(ctx context.Context, tenantID, id int64, status string) error {
	if s.CompleteFunc != nil {
		return s.CompleteFunc(ctx, tenantID, id, status)
	}
	return nil
}

// Ancestors invokes AncestorsFunc when configured.
func (s *WorkItemStore) Ancestors(ctx context.Context, tenantID, id int64) ([]domain.WorkItem, error) {
	if s.AncestorsFunc != nil {
		return s.AncestorsFunc(ctx, tenantID, id)
	}
	return nil, nil
}

var (
	_ contract.DelegationGrantRepository = (*DelegationGrantRepository)(nil)
	_ contract.WorkItemStore             = (*WorkItemStore)(nil)
)
