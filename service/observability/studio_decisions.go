package observability

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioDecisions shows a tenant's decision records to a person or service holding AGENT_AUDIT
// VIEW at the tenant. It fails closed.
type StudioDecisions struct {
	Audit      contract.AuditQuery
	Principals contract.PrincipalResolver
	Roles      contract.PrincipalAuthorizer
}

var _ contract.StudioDecisionsBackend = (*StudioDecisions)(nil)

func (s *StudioDecisions) Decisions(ctx context.Context, actor domain.StudioActor, query domain.DecisionQuery) (domain.DecisionPage, error) {
	if s.Audit == nil || s.Principals == nil || s.Roles == nil {
		return domain.DecisionPage{}, fmt.Errorf("%w: studio decisions need an audit query, principal resolver, and role authorizer", domain.ErrNotReady)
	}
	if actor.TenantID <= 0 || strings.TrimSpace(actor.Principal.ID) == "" ||
		(actor.Principal.Kind != domain.PrincipalHuman && actor.Principal.Kind != domain.PrincipalService) {
		return domain.DecisionPage{}, fmt.Errorf("%w: a studio actor is a person or a service of a tenant", domain.ErrValidation)
	}
	if query.Limit < 0 || query.Before < 0 {
		return domain.DecisionPage{}, fmt.Errorf("%w: before and limit cannot be negative", domain.ErrValidation)
	}
	if query.Outcome != "" && !query.Outcome.Valid() {
		return domain.DecisionPage{}, fmt.Errorf("%w: outcome %q is not an audit_decision_outcome code", domain.ErrValidation, query.Outcome)
	}
	principal, err := s.Principals.Resolve(ctx, actor.TenantID, actor.Principal)
	if err != nil {
		return domain.DecisionPage{}, err
	}
	grant, err := s.Roles.Authorize(ctx, principal, domain.AuditAuthorizationObject, domain.AuditView, domain.AuditAuthorizationValue)
	if err != nil {
		return domain.DecisionPage{}, fmt.Errorf("authorize %s %s: %w", domain.AuditAuthorizationObject, domain.AuditView, err)
	}
	if !grant.Allowed {
		return domain.DecisionPage{}, fmt.Errorf("%w: %s %s on %s", domain.ErrForbidden, domain.AuditAuthorizationObject, domain.AuditView, domain.AuditAuthorizationValue)
	}
	query.TenantID = actor.TenantID
	return s.Audit.Decisions(ctx, query)
}
