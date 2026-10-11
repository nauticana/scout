package controlplane

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

var _ contract.StudioAuthorizer = (*StudioService)(nil)

// agentScopeValue and agentTypeScopeValue are the AGENT_STUDIO grant values; keel matches
// a grant's low_limit exactly or as '*', so a role is granted every agent or named ones.
func agentScopeValue(agentID string) string         { return "agent:" + agentID }
func agentTypeScopeValue(agentTypeID string) string { return "agent_type:" + agentTypeID }

func (s *StudioService) principal(ctx context.Context, actor domain.StudioActor) (domain.Principal, error) {
	if err := validateActor(actor); err != nil {
		return domain.Principal{}, err
	}
	if s.Principals == nil || s.Roles == nil {
		return domain.Principal{}, fmt.Errorf("studio service: principal resolver and role authorizer are required")
	}
	return s.Principals.Resolve(ctx, actor.TenantID, actor.Principal)
}

func (s *StudioService) grant(ctx context.Context, principal domain.Principal, action domain.StudioAction, value string) (domain.AuthorizationGrant, error) {
	grant, err := s.Roles.Authorize(ctx, principal, StudioAuthorizationObject, string(action), value)
	if err != nil {
		return domain.AuthorizationGrant{}, fmt.Errorf("authorize studio %s: %w", action, err)
	}
	return grant, nil
}

// authorizeValue fails closed unless the actor holds action at value.
func (s *StudioService) authorizeValue(ctx context.Context, actor domain.StudioActor, action domain.StudioAction, value string) error {
	principal, err := s.principal(ctx, actor)
	if err != nil {
		return err
	}
	grant, err := s.grant(ctx, principal, action, value)
	if err != nil {
		return err
	}
	if !grant.Allowed {
		return fmt.Errorf("%w: %s %s on %s", domain.ErrForbidden, StudioAuthorizationObject, action, value)
	}
	return nil
}

// Authorize fails closed unless the actor holds action on the agent.
func (s *StudioService) Authorize(ctx context.Context, actor domain.StudioActor, action domain.StudioAction, agentID string) error {
	if agentID == "" {
		return validationError("agent_id", "required")
	}
	return s.authorizeValue(ctx, actor, action, agentScopeValue(agentID))
}

// editable authorizes action on an agent and returns its draft; an archived agent is
// read-only until it is restored.
func (s *StudioService) editable(ctx context.Context, actor domain.StudioActor, action domain.StudioAction, agentID string) (domain.AgentDraft, error) {
	if err := s.Authorize(ctx, actor, action, agentID); err != nil {
		return domain.AgentDraft{}, err
	}
	draft, err := s.getDraft(ctx, actor.TenantID, agentID)
	if err != nil {
		return domain.AgentDraft{}, err
	}
	if draft.Archived {
		return domain.AgentDraft{}, fmt.Errorf("%w: agent %q is archived", domain.ErrConflict, agentID)
	}
	return draft, nil
}

// viewFilter resolves the actor once and answers, per agent, whether it may be listed.
// A wildcard grant admits every agent without a further query.
func (s *StudioService) viewFilter(ctx context.Context, actor domain.StudioActor) (func(agentID string) (bool, error), error) {
	principal, err := s.principal(ctx, actor)
	if err != nil {
		return nil, err
	}
	all := false
	return func(agentID string) (bool, error) {
		if all {
			return true, nil
		}
		grant, err := s.grant(ctx, principal, domain.StudioView, agentScopeValue(agentID))
		if err != nil {
			return false, err
		}
		all = grant.Allowed && grant.BypassScope
		return grant.Allowed, nil
	}, nil
}
