package mcp

import (
	"context"
	"fmt"
	"slices"

	keelmodel "github.com/nauticana/keel/model"
	"github.com/nauticana/keel/oauth/connect"

	"github.com/nauticana/scout/domain"
)

// Tenant is one tenant a caller may act in, as tools/list sees it.
type Tenant struct {
	ID int64
	// Principal is whose grants a call here is checked against: the caller, or
	// the role a delegation acts as.
	Principal keelmodel.Principal
	Delegated bool
	// ActionLevel is the highest MCPToolPolicy.ActionLevel the caller may use here.
	ActionLevel int
}

// TenantView lists the tenants a caller may act in.
type TenantView interface {
	Tenants(ctx context.Context, caller domain.MCPCaller) ([]Tenant, error)
}

// GrantReader reads one principal's grants; keel's port.DatabaseRepository implements it.
type GrantReader interface {
	ActionGrants(ctx context.Context, principal keelmodel.Principal) (keelmodel.GrantSet, error)
}

// SourceReadiness reports a tenant's data sources; keel's connect.ReadinessResolver implements it.
type SourceReadiness interface {
	Resolve(ctx context.Context, partnerID int64) (connect.SourceReadiness, error)
}

// ToolLister narrows a catalog to the tools a caller could call in at least
// one tenant. Listing never authorizes; the backend checks every call.
type ToolLister struct {
	Tenants TenantView
	Grants  GrantReader
	// Sources is optional; without it MCPToolPolicy.Sources is not checked.
	Sources SourceReadiness
	// Delegable reports whether a delegation may reach a grant scope; nil
	// admits every scope. keel's agency.TenantResolver.Delegable fits.
	Delegable func(scope string) bool
}

// tenantView is one tenant with its principal's grants and its sources' readiness.
type tenantView struct {
	Tenant
	grants  keelmodel.GrantSet
	sources connect.SourceReadiness
}

// List returns the definitions some tenant admits; a tool without
// MCPToolPolicy.Tenant is always listed. A host-trusted caller sees every
// definition. When the caller's tenants, grants or sources cannot be read it
// returns definitions unchanged with the error, so a failed lookup never hides
// a tool the call itself would admit.
func (l ToolLister) List(ctx context.Context, caller domain.MCPCaller, definitions []domain.MCPToolDefinition) ([]domain.MCPToolDefinition, error) {
	if caller.HostTrusted || !slices.ContainsFunc(definitions, func(d domain.MCPToolDefinition) bool { return d.Policy.Tenant }) {
		return definitions, nil
	}
	views, err := l.views(ctx, caller, definitions)
	if err != nil {
		return definitions, err
	}
	listed := make([]domain.MCPToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if !definition.Policy.Tenant || slices.ContainsFunc(views, func(view tenantView) bool { return l.admits(definition.Policy, view) }) {
			listed = append(listed, definition)
		}
	}
	return listed, nil
}

// views reads one grant set per distinct principal and, when a tool needs
// sources, one readiness per tenant.
func (l ToolLister) views(ctx context.Context, caller domain.MCPCaller, definitions []domain.MCPToolDefinition) ([]tenantView, error) {
	if l.Tenants == nil || l.Grants == nil {
		return nil, fmt.Errorf("%w: tool lister needs a tenant view and a grant reader", domain.ErrNotReady)
	}
	tenants, err := l.Tenants.Tenants(ctx, caller)
	if err != nil {
		return nil, fmt.Errorf("mcp tool tenants: %w", err)
	}
	needsSources := l.Sources != nil && slices.ContainsFunc(definitions, func(d domain.MCPToolDefinition) bool {
		return d.Policy.Tenant && len(d.Policy.Sources) > 0
	})
	grantSets := map[string]keelmodel.GrantSet{}
	views := make([]tenantView, 0, len(tenants))
	for _, tenant := range tenants {
		view := tenantView{Tenant: tenant}
		key := fmt.Sprintf("%s|%v|%v", tenant.Principal.Kind, tenant.Principal.ID, tenant.Principal.Scope)
		grants, read := grantSets[key]
		if !read {
			if grants, err = l.Grants.ActionGrants(ctx, tenant.Principal); err != nil {
				return nil, fmt.Errorf("mcp tool grants of %s %v: %w", tenant.Principal.Kind, tenant.Principal.ID, err)
			}
			grantSets[key] = grants
		}
		view.grants = grants
		if needsSources {
			if view.sources, err = l.Sources.Resolve(ctx, tenant.ID); err != nil {
				return nil, fmt.Errorf("mcp tool sources of tenant %d: %w", tenant.ID, err)
			}
		}
		views = append(views, view)
	}
	return views, nil
}

func (l ToolLister) admits(policy domain.MCPToolPolicy, view tenantView) bool {
	if view.ActionLevel < policy.ActionLevel || (view.Delegated && policy.OwnTenantOnly) {
		return false
	}
	if view.sources != nil {
		for _, source := range policy.Sources {
			if !view.sources.Ready(source) {
				return false
			}
		}
	}
	if len(policy.Grants) == 0 {
		return true
	}
	for _, grant := range policy.Grants {
		allowed, _ := view.grants.Allows(grant.Object, grant.Action, grant.Scope)
		if view.Delegated && l.Delegable != nil && !l.Delegable(grant.Scope) {
			allowed = false
		}
		if policy.AnyGrant && allowed {
			return true
		}
		if !policy.AnyGrant && !allowed {
			return false
		}
	}
	return !policy.AnyGrant
}
