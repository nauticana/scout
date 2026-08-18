// Package principal resolves the acting subject of a governed operation and
// evaluates it against keel's authorization objects. Agents and humans share one
// authorization model: the same objects, actions, and low/high limits, differing
// only in the assignment table their roles come from.
package principal

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qAgentAuthorization = "scout_agent_authorization"
	qUserAuthorization  = "scout_user_authorization"
	qAgentPrincipal     = "scout_agent_principal"
)

// The two authorization queries differ only in their assignment table, so the
// low/high limit and bypass semantics stay one implementation.
var principalQueries = map[string]string{
	qAgentAuthorization: `
SELECT a.low_limit, a.high_limit, a.bypass_scope
  FROM agent_permission p, authorization_role_permission a
 WHERE p.role_id = a.role_id
   AND p.begda <= CURRENT_TIMESTAMP
   AND (p.endda IS NULL OR p.endda >= CURRENT_TIMESTAMP)
   AND a.is_active IS TRUE
   AND a.authorization_object_id = ?
   AND a.action = ?
   AND p.tenant_id = ?
   AND p.agent_id = ?
   AND (a.low_limit = ? OR a.low_limit = '*')`,
	qUserAuthorization: `
SELECT a.low_limit, a.high_limit, a.bypass_scope
  FROM user_permission p, authorization_role_permission a
 WHERE p.role_id = a.role_id
   AND p.begda <= CURRENT_TIMESTAMP
   AND (p.endda IS NULL OR p.endda >= CURRENT_TIMESTAMP)
   AND a.is_active IS TRUE
   AND a.authorization_object_id = ?
   AND a.action = ?
   AND p.user_id = ?
   AND (a.low_limit = ? OR a.low_limit = '*')`,
	qAgentPrincipal: `
SELECT p.agent_type_id, p.state_code, d.stable_version
  FROM agent_profile p
  LEFT JOIN agent_deployment d ON d.tenant_id = p.tenant_id AND d.agent_id = p.agent_id
 WHERE p.tenant_id = ? AND p.agent_id = ?`,
}

// RoleAuthorizer answers one authorization-object question for either principal
// kind. A wildcard grant returns BypassScope; an exact grant is value-scoped.
type RoleAuthorizer struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

func (a *RoleAuthorizer) init(ctx context.Context) error {
	if a.DB == nil {
		return fmt.Errorf("role authorizer: database is required")
	}
	a.once.Do(func() { a.qs = a.DB.GetQueryService(ctx, principalQueries) })
	if a.qs == nil {
		return fmt.Errorf("role authorizer: query service is required")
	}
	return nil
}

// Authorize evaluates principal against one object, action, and scope value.
func (a *RoleAuthorizer) Authorize(ctx context.Context, principal domain.Principal, object, action, value string) (domain.AuthorizationGrant, error) {
	if err := a.init(ctx); err != nil {
		return domain.AuthorizationGrant{}, err
	}
	if err := validate(principal); err != nil {
		return domain.AuthorizationGrant{}, err
	}
	if strings.TrimSpace(object) == "" || strings.TrimSpace(action) == "" {
		return domain.AuthorizationGrant{}, fmt.Errorf("%w: authorization object and action are required", domain.ErrValidation)
	}

	var result *keelmodel.QueryResult
	var err error
	switch principal.Kind {
	case domain.PrincipalAgent, domain.PrincipalService:
		result, err = a.qs.Query(ctx, qAgentAuthorization, object, action, principal.TenantID, principal.ID, value)
	case domain.PrincipalHuman:
		userID, convErr := strconv.ParseInt(principal.ID, 10, 64)
		if convErr != nil {
			return domain.AuthorizationGrant{}, fmt.Errorf("%w: human principal id must be a user account id", domain.ErrValidation)
		}
		result, err = a.qs.Query(ctx, qUserAuthorization, object, action, userID, value)
	default:
		return domain.AuthorizationGrant{}, fmt.Errorf("%w: unknown principal kind %q", domain.ErrValidation, principal.Kind)
	}
	if err != nil {
		return domain.AuthorizationGrant{}, fmt.Errorf("check authorization: %w", err)
	}

	grant := domain.AuthorizationGrant{}
	for _, row := range result.Rows {
		low := common.AsString(row[0])
		candidate := domain.AuthorizationGrant{
			Allowed: true, LowLimit: low, HighLimit: common.AsString(row[1]),
			BypassScope: low == "*" || common.AsBool(row[2]),
		}
		if low == value {
			return candidate, nil
		}
		grant = candidate
	}
	return grant, nil
}

func validate(principal domain.Principal) error {
	if principal.Kind == "" || strings.TrimSpace(principal.ID) == "" || principal.TenantID <= 0 {
		return fmt.Errorf("%w: principal requires a kind, id, and tenant", domain.ErrPrincipalUnknown)
	}
	return nil
}

var _ contract.PrincipalAuthorizer = (*RoleAuthorizer)(nil)
