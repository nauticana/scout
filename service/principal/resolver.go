package principal

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// TableResolver turns an authenticated identity into a principal. An agent
// resolves through agent_profile and its deployment, so a disabled agent never
// produces a usable principal.
type TableResolver struct {
	DB keelport.DatabaseRepository
	// Scopes is optional; without it every principal resolves to the tenant root.
	Scopes contract.ScopeRepository
	// RootScopeID is the tenant root a principal defaults to.
	RootScopeID string

	once sync.Once
	qs   keelport.QueryService
}

func (r *TableResolver) init(ctx context.Context) error {
	if r.DB == nil {
		return fmt.Errorf("principal resolver: database is required")
	}
	r.once.Do(func() { r.qs = r.DB.GetQueryService(ctx, principalQueries) })
	if r.qs == nil {
		return fmt.Errorf("principal resolver: query service is required")
	}
	return nil
}

// Resolve returns the principal for a verified identity, failing closed.
func (r *TableResolver) Resolve(ctx context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error) {
	if err := r.init(ctx); err != nil {
		return domain.Principal{}, err
	}
	if tenantID <= 0 || strings.TrimSpace(ref.ID) == "" {
		return domain.Principal{}, fmt.Errorf("%w: tenant and subject id are required", domain.ErrPrincipalUnknown)
	}
	principal := domain.Principal{Kind: ref.Kind, ID: ref.ID, TenantID: tenantID, ScopeID: r.RootScopeID}
	switch ref.Kind {
	case domain.PrincipalHuman:
		if _, err := strconv.ParseInt(ref.ID, 10, 64); err != nil {
			return domain.Principal{}, fmt.Errorf("%w: human subject %q is not a user account id", domain.ErrPrincipalUnknown, ref.ID)
		}
		return principal, nil
	case domain.PrincipalAgent, domain.PrincipalService:
		result, err := r.qs.Query(ctx, qAgentPrincipal, tenantID, ref.ID)
		if err != nil {
			return domain.Principal{}, fmt.Errorf("resolve agent principal: %w", err)
		}
		if len(result.Rows) == 0 {
			return domain.Principal{}, fmt.Errorf("%w: agent %q", domain.ErrPrincipalUnknown, ref.ID)
		}
		row := result.Rows[0]
		if domain.AgentState(common.AsString(row[1])) != domain.AgentStateActive {
			return domain.Principal{}, fmt.Errorf("%w: agent %q is not active", domain.ErrForbidden, ref.ID)
		}
		principal.Release = common.AsString(row[2])
		return principal, nil
	default:
		return domain.Principal{}, fmt.Errorf("%w: unknown principal kind %q", domain.ErrPrincipalUnknown, ref.Kind)
	}
}

// ChainVerifier validates an authority chain before a delegated principal acts.
// It checks shape, depth, and validity windows; whether each grantor actually
// held what it conveyed is the delegation grant's own check, added with DWF-9.
type ChainVerifier struct {
	Grants contract.DelegationGrantRepository
	// MaxDepth bounds the chain regardless of what a grant conveys.
	MaxDepth int
	Now      func() time.Time
}

// DefaultMaxDelegationDepth bounds a chain when no flag value is supplied.
const DefaultMaxDelegationDepth = 4

// Verify returns the authority the principal exercises, or the reason it cannot.
func (v *ChainVerifier) Verify(ctx context.Context, principal domain.Principal) (domain.AuthorityRef, error) {
	if err := validate(principal); err != nil {
		return domain.AuthorityRef{}, err
	}
	subject := domain.PrincipalRef{Kind: principal.Kind, ID: principal.ID}
	if len(principal.Authority) == 0 {
		return domain.AuthorityRef{Subject: subject}, nil
	}
	if v.Grants == nil {
		return domain.AuthorityRef{}, fmt.Errorf("delegation verifier: grant repository is required for a delegated principal")
	}
	limit := v.MaxDepth
	if limit <= 0 {
		limit = DefaultMaxDelegationDepth
	}
	if len(principal.Authority) > limit {
		return domain.AuthorityRef{}, fmt.Errorf("%w: chain is %d hops, the limit is %d", domain.ErrDelegationDepth, len(principal.Authority), limit)
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	for index, hop := range principal.Authority {
		if strings.TrimSpace(hop.GrantID) == "" || strings.TrimSpace(hop.Grantor.ID) == "" {
			return domain.AuthorityRef{}, fmt.Errorf("%w: hop %d names no grant or grantor", domain.ErrValidation, index)
		}
		if !hop.NotBefore.IsZero() && now.Before(hop.NotBefore) {
			return domain.AuthorityRef{}, fmt.Errorf("%w: grant %s is not yet valid", domain.ErrGrantExpired, hop.GrantID)
		}
		if !hop.NotAfter.IsZero() && !now.Before(hop.NotAfter) {
			return domain.AuthorityRef{}, fmt.Errorf("%w: grant %s ended at %s", domain.ErrGrantExpired, hop.GrantID, hop.NotAfter.UTC())
		}
		grant, err := v.Grants.Get(ctx, principal.TenantID, hop.GrantID)
		if err != nil {
			return domain.AuthorityRef{}, fmt.Errorf("verify authority hop %s: %w", hop.GrantID, err)
		}
		expectedGrantee := subject
		if index > 0 {
			expectedGrantee = principal.Authority[index-1].Grantor
		}
		if grant.Grantee != expectedGrantee || grant.Grantor != hop.Grantor ||
			grant.MaxDepth != hop.MaxDepth || grant.BudgetMinorUnits != hop.BudgetMinorUnits ||
			grant.Currency != hop.Currency || grant.ApprovalRequired != hop.ApprovalRequired ||
			!grant.ValidFrom.Equal(hop.NotBefore) || !grant.ValidTo.Equal(hop.NotAfter) {
			return domain.AuthorityRef{}, fmt.Errorf("%w: authority hop %s does not match its stored grant", domain.ErrForbidden, hop.GrantID)
		}
		if !inForce(grant, now) {
			return domain.AuthorityRef{}, fmt.Errorf("%w: grant %s is no longer in force", domain.ErrGrantExpired, hop.GrantID)
		}
		// Remaining hops are the delegation this one still had to spend.
		if remaining := len(principal.Authority) - index - 1; hop.MaxDepth < remaining {
			return domain.AuthorityRef{}, fmt.Errorf("%w: grant %s conveys depth %d but %d hops follow", domain.ErrDelegationDepth, hop.GrantID, hop.MaxDepth, remaining)
		}
	}
	immediate := principal.Authority[0]
	return domain.AuthorityRef{Subject: subject, GrantID: immediate.GrantID, Grantor: immediate.Grantor}, nil
}

var (
	_ contract.PrincipalResolver  = (*TableResolver)(nil)
	_ contract.DelegationVerifier = (*ChainVerifier)(nil)
)
