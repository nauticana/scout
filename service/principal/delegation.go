package principal

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qGrantPut      = "scout_delegation_grant_put"
	qGrantGet      = "scout_delegation_grant_get"
	qGrantRevoke   = "scout_delegation_grant_revoke"
	qGrantsGrantee = "scout_delegation_grants_grantee"

	grantColumns = `grant_id, grantor_kind, grantor_user_id, grantor_agent_id, grantee_kind, grantee_agent_id,
       action_scope, max_depth, budget_minor_units, currency_code, approval_required, begda, endda, revoked_at`
)

var delegationQueries = map[string]string{
	qGrantPut: `
INSERT INTO delegation_grant
       (tenant_id, grant_id, grantor_kind, grantor_user_id, grantor_agent_id, grantee_kind, grantee_agent_id,
        action_scope, max_depth, budget_minor_units, currency_code, approval_required, begda, endda, granted_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qGrantGet: `
SELECT ` + grantColumns + `
  FROM delegation_grant
 WHERE tenant_id = ? AND grant_id = ?`,
	qGrantRevoke: `
UPDATE delegation_grant
   SET revoked_at = CURRENT_TIMESTAMP, endda = COALESCE(endda, CURRENT_TIMESTAMP)
 WHERE tenant_id = ? AND grant_id = ? AND revoked_at IS NULL
RETURNING grant_id`,
	qGrantsGrantee: `
SELECT ` + grantColumns + `
  FROM delegation_grant
 WHERE tenant_id = ? AND grantee_kind = ? AND grantee_agent_id = ?
   AND revoked_at IS NULL AND begda <= CURRENT_TIMESTAMP AND (endda IS NULL OR endda > CURRENT_TIMESTAMP)`,
}

// TableGrants stores delegation bounds through keel named SQL.
type TableGrants struct {
	DB  keelport.DatabaseRepository
	Now func() time.Time

	once sync.Once
	qs   keelport.QueryService
}

func (g *TableGrants) init(ctx context.Context) error {
	if g.DB == nil {
		return fmt.Errorf("delegation grants: database is required")
	}
	g.once.Do(func() { g.qs = g.DB.GetQueryService(ctx, delegationQueries) })
	if g.qs == nil {
		return fmt.Errorf("delegation grants: query service is required")
	}
	return nil
}

// Put records a grant.
func (g *TableGrants) Put(ctx context.Context, grant domain.DelegationGrant) error {
	if err := g.init(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(grant.GrantID) == "" || grant.Grantee.ID == "" || grant.Grantor.ID == "" {
		return fmt.Errorf("%w: a grant needs an id, a grantor, and a grantee", domain.ErrValidation)
	}
	if grant.MaxDepth < 0 {
		return fmt.Errorf("%w: grant depth cannot be negative", domain.ErrValidation)
	}
	if grant.TenantID <= 0 {
		return fmt.Errorf("%w: a grant needs its tenant", domain.ErrValidation)
	}
	if grant.Grantee.Kind != domain.PrincipalAgent {
		return fmt.Errorf("%w: delegation grantees must be agents", domain.ErrValidation)
	}
	if grant.Grantor.Kind != domain.PrincipalHuman && grant.Grantor.Kind != domain.PrincipalAgent {
		return fmt.Errorf("%w: delegation grantor kind %q is unsupported", domain.ErrValidation, grant.Grantor.Kind)
	}
	var grantorUser, grantorAgent any
	if grant.Grantor.Kind == domain.PrincipalHuman {
		grantorUser = grant.Grantor.ID
	} else {
		grantorAgent = grant.Grantor.ID
	}
	var budget, currency any
	if grant.BudgetMinorUnits > 0 {
		budget, currency = grant.BudgetMinorUnits, grant.Currency
	}
	_, err := g.qs.Query(context.WithoutCancel(ctx), qGrantPut, grant.TenantID, grant.GrantID,
		string(grant.Grantor.Kind), grantorUser, grantorAgent,
		string(grant.Grantee.Kind), grant.Grantee.ID, grant.ActionScope, grant.MaxDepth,
		budget, currency, grant.ApprovalRequired, grant.ValidFrom, nullTime(grant.ValidTo), nil)
	if err != nil {
		return fmt.Errorf("put delegation grant: %w", err)
	}
	return nil
}

// Get returns one grant regardless of its validity window; the caller checks it.
func (g *TableGrants) Get(ctx context.Context, tenantID int64, grantID string) (domain.DelegationGrant, error) {
	if err := g.init(ctx); err != nil {
		return domain.DelegationGrant{}, err
	}
	result, err := g.qs.Query(ctx, qGrantGet, tenantID, grantID)
	if err != nil {
		return domain.DelegationGrant{}, fmt.Errorf("get delegation grant: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.DelegationGrant{}, fmt.Errorf("%w: delegation grant %q", domain.ErrNotFound, grantID)
	}
	return scanGrant(tenantID, result.Rows[0]), nil
}

// Revoke ends a grant immediately.
func (g *TableGrants) Revoke(ctx context.Context, tenantID int64, grantID, _ string) error {
	if err := g.init(ctx); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	revoked, err := g.qs.Query(ctx, qGrantRevoke, tenantID, grantID)
	if err != nil {
		return fmt.Errorf("revoke delegation grant: %w", err)
	}
	if len(revoked.Rows) == 0 {
		return fmt.Errorf("%w: grant %q is not open", domain.ErrConflict, grantID)
	}
	return nil
}

// ForGrantee lists the grants a principal currently holds.
func (g *TableGrants) ForGrantee(ctx context.Context, tenantID int64, grantee domain.PrincipalRef) ([]domain.DelegationGrant, error) {
	if err := g.init(ctx); err != nil {
		return nil, err
	}
	result, err := g.qs.Query(ctx, qGrantsGrantee, tenantID, string(grantee.Kind), grantee.ID)
	if err != nil {
		return nil, fmt.Errorf("list delegation grants: %w", err)
	}
	grants := make([]domain.DelegationGrant, 0, len(result.Rows))
	for _, row := range result.Rows {
		grants = append(grants, scanGrant(tenantID, row))
	}
	return grants, nil
}

// GrantAuthorizer decides whether a delegation may happen and what bounds it
// conveys. It enforces the rule that makes the whole model safe: a grant may
// convey no more than its grantor holds, checked as a subset over the same
// authorization objects both principal kinds evaluate through.
type GrantAuthorizer struct {
	Grants     contract.DelegationGrantRepository
	Authorizer contract.PrincipalAuthorizer
	// AuthorizationObject is the object delegated actions are checked against.
	AuthorizationObject string
	Now                 func() time.Time
}

// Authorize returns the bounds the grantee inherits for one action.
func (a *GrantAuthorizer) Authorize(ctx context.Context, delegator domain.Principal, grantee domain.PrincipalRef, action string) (domain.DelegationAuthorization, error) {
	if a.Grants == nil || a.Authorizer == nil {
		return domain.DelegationAuthorization{}, fmt.Errorf("grant authorizer: grants and a principal authorizer are required")
	}
	if err := validate(delegator); err != nil {
		return domain.DelegationAuthorization{}, err
	}
	if grantee.Kind != domain.PrincipalAgent || strings.TrimSpace(grantee.ID) == "" {
		return domain.DelegationAuthorization{}, fmt.Errorf("%w: delegation target must be an agent", domain.ErrValidation)
	}
	grants, err := a.Grants.ForGrantee(ctx, delegator.TenantID, grantee)
	if err != nil {
		return domain.DelegationAuthorization{}, err
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	for _, grant := range grants {
		if grant.Grantor.ID != delegator.ID || grant.Grantor.Kind != delegator.Kind {
			continue
		}
		if !inForce(grant, now) || !actionCovered(grant.ActionScope, action) {
			continue
		}
		// The grantor must still hold what it is passing on; a revoked or expired
		// role must not keep flowing through an older grant.
		held, err := a.Authorizer.Authorize(ctx, delegator, a.AuthorizationObject, action, grant.ActionScope)
		if err != nil {
			return domain.DelegationAuthorization{}, err
		}
		if !held.Allowed {
			return domain.DelegationAuthorization{}, fmt.Errorf("%w: grantor %q no longer holds %q", domain.ErrAuthorityExceeded, delegator.ID, action)
		}
		bounds := domain.DelegationBounds{
			RemainingDepth: grant.MaxDepth, BudgetMinorUnits: grant.BudgetMinorUnits, Currency: grant.Currency,
			ScopeID: delegator.ScopeID, ApprovalRequired: grant.ApprovalRequired,
		}
		return domain.DelegationAuthorization{
			GrantID: grant.GrantID, Bounds: bounds,
			Authority: domain.AuthorityHop{
				GrantID: grant.GrantID, Grantor: grant.Grantor, MaxDepth: grant.MaxDepth,
				BudgetMinorUnits: grant.BudgetMinorUnits, Currency: grant.Currency,
				ApprovalRequired: grant.ApprovalRequired, NotBefore: grant.ValidFrom, NotAfter: grant.ValidTo,
			},
		}, nil
	}
	return domain.DelegationAuthorization{}, fmt.Errorf("%w: no grant lets %q delegate %q to %q",
		domain.ErrForbidden, delegator.ID, action, grantee.ID)
}

// Narrow returns the bounds one hop passes to the next. Every field may only
// shrink, so a deeper hop can never regain depth, budget, or a wider scope.
func Narrow(parent, child domain.DelegationBounds) (domain.DelegationBounds, error) {
	if parent.RemainingDepth <= 0 {
		return domain.DelegationBounds{}, fmt.Errorf("%w: no delegation depth remains", domain.ErrDelegationDepth)
	}
	next := domain.DelegationBounds{
		RemainingDepth:   min(parent.RemainingDepth-1, child.RemainingDepth),
		Currency:         firstBound(parent.Currency, child.Currency),
		ScopeID:          firstBound(parent.ScopeID, child.ScopeID),
		ApprovalRequired: parent.ApprovalRequired || child.ApprovalRequired,
	}
	switch {
	case parent.BudgetMinorUnits <= 0:
		next.BudgetMinorUnits = child.BudgetMinorUnits
	case child.BudgetMinorUnits <= 0 || child.BudgetMinorUnits > parent.BudgetMinorUnits:
		next.BudgetMinorUnits = parent.BudgetMinorUnits
	default:
		next.BudgetMinorUnits = child.BudgetMinorUnits
	}
	if child.ScopeID != "" && child.ScopeID != parent.ScopeID && parent.ScopeID != "" {
		return domain.DelegationBounds{}, fmt.Errorf("%w: a hop cannot move work to scope %q", domain.ErrAuthorityExceeded, child.ScopeID)
	}
	if child.Currency != "" && parent.Currency != "" && child.Currency != parent.Currency {
		return domain.DelegationBounds{}, fmt.Errorf("%w: a hop cannot change budget currency", domain.ErrValidation)
	}
	return next, nil
}

func firstBound(parent, child string) string {
	if parent != "" {
		return parent
	}
	return child
}

func inForce(grant domain.DelegationGrant, now time.Time) bool {
	if !grant.RevokedAt.IsZero() {
		return false
	}
	if !grant.ValidFrom.IsZero() && now.Before(grant.ValidFrom) {
		return false
	}
	return grant.ValidTo.IsZero() || now.Before(grant.ValidTo)
}

// actionCovered matches exactly or on a single trailing "*", like policy actions.
func actionCovered(scope, action string) bool {
	if scope == "*" || scope == action {
		return true
	}
	prefix, wildcard := strings.CutSuffix(scope, "*")
	return wildcard && strings.HasPrefix(action, prefix)
}

func scanGrant(tenantID int64, row []any) domain.DelegationGrant {
	grantorKind := domain.PrincipalKind(common.AsString(row[1]))
	grantorID := common.AsString(row[3])
	if grantorKind == domain.PrincipalHuman {
		grantorID = common.AsString(row[2])
	}
	validTo, _ := common.AsTimeOK(row[12])
	revoked, _ := common.AsTimeOK(row[13])
	return domain.DelegationGrant{
		TenantID: tenantID, GrantID: common.AsString(row[0]),
		Grantor:     domain.PrincipalRef{Kind: grantorKind, ID: grantorID},
		Grantee:     domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[4])), ID: common.AsString(row[5])},
		ActionScope: common.AsString(row[6]), MaxDepth: int(common.AsInt64(row[7])),
		BudgetMinorUnits: common.AsInt64(row[8]), Currency: common.AsString(row[9]),
		ApprovalRequired: common.AsBool(row[10]), ValidFrom: common.AsTime(row[11]),
		ValidTo: validTo, RevokedAt: revoked,
	}
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

var (
	_ contract.DelegationGrantRepository = (*TableGrants)(nil)
	_ contract.DelegationAuthorizer      = (*GrantAuthorizer)(nil)
)
