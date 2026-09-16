// Package principal resolves the acting subject of a governed operation and
// evaluates it against keel's authorization objects. Agents and humans share one
// authorization model: the same objects, actions, and low/high limits, differing
// only in the assignment table their roles come from.
package principal

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"

	charterkeel "github.com/nauticana/charter/sdk/adapter/keel"
	"github.com/nauticana/keel/common"
	keeldata "github.com/nauticana/keel/data"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const qAgentPrincipal = "scout_agent_principal"

// agentGrants is where an agent or service principal's role grants live. keel
// generates the lookup SQL from it, so agents and humans cannot drift apart.
var agentGrants = keeldata.GrantSource{Table: "agent_permission", Subject: "agent_id", Filters: []string{"tenant_id"}}

var principalQueries = map[string]string{
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
	// Grants resolves each kind's grant SQL; nil registers Scout's agent and
	// service kinds on a fresh keel catalog.
	Grants keelport.GrantCatalog

	once    sync.Once
	qs      keelport.QueryService
	initErr error
}

// newGrantCatalog registers Scout's non-human kinds alongside keel's human one.
func newGrantCatalog() (keelport.GrantCatalog, error) {
	catalog := keeldata.NewGrantCatalog()
	// Agents register under Charter's kind so a Charter PermissionGate and Scout
	// evaluate the same grants; services are Scout-only and share the table.
	for _, kind := range []keelmodel.PrincipalKind{charterkeel.AgentPrincipalKind, keelmodel.PrincipalKind(domain.PrincipalService)} {
		if err := catalog.Register(kind, agentGrants); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

func (a *RoleAuthorizer) init(ctx context.Context) error {
	if a.DB == nil {
		return fmt.Errorf("role authorizer: database is required")
	}
	a.once.Do(func() {
		if a.Grants == nil {
			catalog, err := newGrantCatalog()
			if err != nil {
				a.initErr = fmt.Errorf("role authorizer: %w", err)
				return
			}
			a.Grants = catalog
		}
		queries := make(map[string]string, len(principalQueries)+6)
		maps.Copy(queries, principalQueries)
		maps.Copy(queries, a.Grants.Queries())
		a.qs = a.DB.GetQueryService(ctx, queries)
	})
	if a.initErr != nil {
		return a.initErr
	}
	if a.qs == nil {
		return fmt.Errorf("role authorizer: query service is required")
	}
	return nil
}

// keelPrincipal maps a Scout principal onto the subject keel binds into the
// grant query. A human is keel's built-in kind, keyed by user_account.id.
func keelPrincipal(principal domain.Principal) (keelmodel.Principal, error) {
	switch principal.Kind {
	case domain.PrincipalAgent, domain.PrincipalService:
		return keelmodel.Principal{
			Kind:  keelmodel.PrincipalKind(principal.Kind),
			ID:    principal.ID,
			Scope: []any{principal.TenantID},
		}, nil
	case domain.PrincipalHuman:
		userID, err := strconv.ParseInt(principal.ID, 10, 64)
		if err != nil {
			return keelmodel.Principal{}, fmt.Errorf("%w: human principal id must be a user account id", domain.ErrValidation)
		}
		return keelmodel.Principal{Kind: keelmodel.PrincipalUser, ID: userID}, nil
	default:
		return keelmodel.Principal{}, fmt.Errorf("%w: unknown principal kind %q", domain.ErrValidation, principal.Kind)
	}
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

	subject, err := keelPrincipal(principal)
	if err != nil {
		return domain.AuthorizationGrant{}, err
	}
	args, err := a.Grants.Args(subject)
	if err != nil {
		return domain.AuthorizationGrant{}, fmt.Errorf("%w: %s", domain.ErrValidation, err)
	}
	args = append([]any{object, action}, append(args, value)...)
	result, err := a.qs.Query(ctx, a.Grants.CheckQuery(subject.Kind), args...)
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
