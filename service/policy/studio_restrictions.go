package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DefaultStudioRestrictionEntries bounds the denials, and separately the guardrail rules, of
// a tenant layer written through Studio.
const DefaultStudioRestrictionEntries = 256

// StudioRestrictions shows Studio the platform's restriction layer, read-only, and the
// tenant's, which a person or service holding AGENT_RESTRICTION REPLACE_TENANT replaces as a
// whole behind its digest. For an agent it also names the effective settings each restriction
// overrides. It authorizes every call itself and fails closed.
type StudioRestrictions struct {
	Layers contract.RestrictionLayerReader
	Writer contract.RestrictionLayerWriter
	// Principals and Roles authorize AGENT_RESTRICTION at the tenant-wide value.
	Principals contract.PrincipalResolver
	Roles      contract.PrincipalAuthorizer
	// Studio compiles an agent's settings and authorizes AGENT_STUDIO VIEW on the agent.
	Studio contract.AgentStudioHTTPBackend
	// MaxEntries bounds the denials and the guardrail rules of a tenant layer; zero takes
	// DefaultStudioRestrictionEntries.
	MaxEntries int
}

var _ contract.StudioRestrictionBackend = (*StudioRestrictions)(nil)

func (s *StudioRestrictions) ready() error {
	if s.Layers == nil || s.Writer == nil || s.Principals == nil || s.Roles == nil || s.Studio == nil {
		return fmt.Errorf("studio restrictions: restriction reader and writer, principal resolver, role authorizer, and studio are required")
	}
	if s.MaxEntries < 0 {
		return fmt.Errorf("%w: studio restrictions: max entries cannot be negative", domain.ErrValidation)
	}
	return nil
}

// Restrictions returns both layers in force for the actor's tenant; with an agent, also the
// restrictions that override its effective settings.
func (s *StudioRestrictions) Restrictions(ctx context.Context, actor domain.StudioActor, agentID string) (domain.RestrictionOverview, error) {
	if err := s.ready(); err != nil {
		return domain.RestrictionOverview{}, err
	}
	if _, err := s.authorize(ctx, actor, domain.RestrictionView); err != nil {
		return domain.RestrictionOverview{}, err
	}
	layers, err := s.Layers.Layers(ctx, actor.TenantID)
	if err != nil {
		return domain.RestrictionOverview{}, err
	}
	overview := domain.RestrictionOverview{Layers: layers, AgentID: agentID}
	if agentID == "" {
		return overview, nil
	}
	settings, err := s.Studio.Settings(ctx, actor, agentID)
	if err != nil {
		return domain.RestrictionOverview{}, err
	}
	if settings.Problem != "" {
		overview.SettingsProblem = settings.Problem
		return overview, nil
	}
	overview.Overrides, err = restrictionOverrides(layers, settings.Effective)
	if errors.Is(err, domain.ErrValidation) {
		overview.SettingsProblem = err.Error()
		return overview, nil
	}
	if err != nil {
		return domain.RestrictionOverview{}, err
	}
	return overview, nil
}

// ReplaceTenant replaces the tenant's layer as a whole when it is still at expectedDigest,
// empty before the first write, and returns the layer written in canonical order.
func (s *StudioRestrictions) ReplaceTenant(ctx context.Context, actor domain.StudioActor, layer domain.RestrictionLayer, expectedDigest string) (domain.RestrictionLayer, error) {
	if err := s.ready(); err != nil {
		return domain.RestrictionLayer{}, err
	}
	principal, err := s.authorize(ctx, actor, domain.RestrictionReplaceTenant)
	if err != nil {
		return domain.RestrictionLayer{}, err
	}
	limit := s.MaxEntries
	if limit == 0 {
		limit = DefaultStudioRestrictionEntries
	}
	if len(layer.Denials) > limit || len(layer.Guardrails) > limit {
		return domain.RestrictionLayer{}, fmt.Errorf("%w: a tenant restriction layer holds at most %d denials and %d guardrail rules", domain.ErrValidation, limit, limit)
	}
	canonical, err := canonicalLayer(layer)
	if err != nil {
		return domain.RestrictionLayer{}, err
	}
	digest, err := s.Writer.ReplaceTenant(ctx, principal, actor.TenantID, layer, expectedDigest)
	if err != nil {
		return domain.RestrictionLayer{}, err
	}
	written := canonical.layer
	written.Digest = digest
	return written, nil
}

// authorize resolves the actor, a person or a service, and fails closed unless it holds
// action on AGENT_RESTRICTION at the tenant-wide value.
func (s *StudioRestrictions) authorize(ctx context.Context, actor domain.StudioActor, action domain.RestrictionAction) (domain.Principal, error) {
	switch {
	case actor.TenantID <= 0 || strings.TrimSpace(actor.Principal.ID) == "":
		return domain.Principal{}, fmt.Errorf("%w: tenant and actor are required", domain.ErrValidation)
	case actor.Principal.Kind == domain.PrincipalService:
	case actor.Principal.Kind == domain.PrincipalHuman && userAccountID(actor.Principal.ID):
	default:
		return domain.Principal{}, fmt.Errorf("%w: a studio actor is a person with a user account id or a service", domain.ErrValidation)
	}
	principal, err := s.Principals.Resolve(ctx, actor.TenantID, actor.Principal)
	if err != nil {
		return domain.Principal{}, err
	}
	grant, err := s.Roles.Authorize(ctx, principal, domain.RestrictionAuthorizationObject, string(action), domain.RestrictionAuthorizationValue)
	if err != nil {
		return domain.Principal{}, fmt.Errorf("authorize restriction %s: %w", action, err)
	}
	if !grant.Allowed {
		return domain.Principal{}, fmt.Errorf("%w: %s %s on %s", domain.ErrForbidden, domain.RestrictionAuthorizationObject, action, domain.RestrictionAuthorizationValue)
	}
	return principal, nil
}
