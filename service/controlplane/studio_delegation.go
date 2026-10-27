package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
)

// StudioDelegations lets Studio show and grant which agents may hand work to which.
// A grant only narrows: it is bounded in depth and time, never names its own grantor
// or an agent already upstream of it, and at use the grant authorizer still checks
// that the grantor holds what the grant conveys.
type StudioDelegations struct {
	Authorizer contract.StudioAuthorizer
	Grants     contract.DelegationGrantRepository
	// MaxDepth bounds the re-delegation a grant may convey; it is agent_max_delegation_hops.
	MaxDepth int
	// Entropy generates grant ids; nil uses crypto/rand.
	Entropy io.Reader
	Now     func() time.Time
}

var _ contract.StudioDelegationBackend = (*StudioDelegations)(nil)

func (s *StudioDelegations) ready() error {
	if s.Authorizer == nil || s.Grants == nil {
		return fmt.Errorf("studio delegations: authorizer and grant repository are required")
	}
	return nil
}

// List returns the grants in force that the agent holds or conveys.
func (s *StudioDelegations) List(ctx context.Context, actor domain.StudioActor, agentID string) ([]domain.DelegationGrant, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioView, agentID); err != nil {
		return nil, err
	}
	return s.Grants.ForAgent(ctx, actor.TenantID, agentID)
}

// Grant lets the grantor agent hand work to the grantee within the grant's bounds.
func (s *StudioDelegations) Grant(ctx context.Context, actor domain.StudioActor, grant domain.DelegationGrant) (domain.DelegationGrant, error) {
	if err := s.ready(); err != nil {
		return domain.DelegationGrant{}, err
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioEdit, grant.Grantor.ID); err != nil {
		return domain.DelegationGrant{}, err
	}
	// A grant also holds the grantee, so its author must be able to see it.
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioView, grant.Grantee.ID); err != nil {
		return domain.DelegationGrant{}, err
	}
	now := s.now()
	grant.TenantID, grant.Grantor.Kind, grant.Grantee.Kind = actor.TenantID, domain.PrincipalAgent, domain.PrincipalAgent
	if grant.ValidFrom.IsZero() {
		grant.ValidFrom = now
	}
	if err := s.validate(grant, now); err != nil {
		return domain.DelegationGrant{}, err
	}
	if err := s.refuseCycle(ctx, actor.TenantID, grant); err != nil {
		return domain.DelegationGrant{}, err
	}
	id, err := s.grantID()
	if err != nil {
		return domain.DelegationGrant{}, err
	}
	grant.GrantID = id
	if err = s.Grants.Put(ctx, grant); err != nil {
		return domain.DelegationGrant{}, err
	}
	return grant, nil
}

// Revoke ends one of the agent's own grants at once.
func (s *StudioDelegations) Revoke(ctx context.Context, actor domain.StudioActor, agentID, grantID string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := s.Authorizer.Authorize(ctx, actor, domain.StudioEdit, agentID); err != nil {
		return err
	}
	grant, err := s.Grants.Get(ctx, actor.TenantID, grantID)
	if err != nil {
		return err
	}
	if grant.Grantor != (domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: agentID}) {
		return fmt.Errorf("%w: grant %q is not conveyed by agent %q", domain.ErrNotFound, grantID, agentID)
	}
	return s.Grants.Revoke(ctx, actor.TenantID, grantID, "revoked in studio by "+string(actor.Principal.Kind)+":"+actor.Principal.ID)
}

func (s *StudioDelegations) validate(grant domain.DelegationGrant, now time.Time) error {
	var fields []domain.AgentFieldError
	if grant.Grantee.ID == "" || grant.Grantee.ID == grant.Grantor.ID {
		fields = append(fields, domain.AgentFieldError{Field: "grantee", Message: "must be another agent"})
	}
	if strings.TrimSpace(grant.ActionScope) == "" || len(grant.ActionScope) > 200 {
		fields = append(fields, domain.AgentFieldError{Field: "action_scope", Message: "must contain 1 to 200 characters"})
	}
	if grant.MaxDepth < 0 || grant.MaxDepth > s.MaxDepth {
		fields = append(fields, domain.AgentFieldError{Field: "max_depth", Message: fmt.Sprintf("must be between 0 and %d", s.MaxDepth)})
	}
	if grant.BudgetMinorUnits < 0 || grant.BudgetMinorUnits > 0 && len(grant.Currency) != 3 {
		fields = append(fields, domain.AgentFieldError{Field: "budget_minor_units", Message: "must not be negative, and a budget names its currency"})
	}
	if grant.ValidTo.IsZero() || !grant.ValidTo.After(grant.ValidFrom) || !grant.ValidTo.After(now) {
		fields = append(fields, domain.AgentFieldError{Field: "valid_to", Message: "a grant must end, after it begins and in the future"})
	}
	if len(fields) > 0 {
		return &StudioValidationError{Fields: fields}
	}
	return nil
}

// refuseCycle walks the grants the grantee conveys, bounded by MaxDepth hops, and refuses
// a grant that would let work return to its grantor.
func (s *StudioDelegations) refuseCycle(ctx context.Context, tenantID int64, grant domain.DelegationGrant) error {
	frontier, seen := []string{grant.Grantee.ID}, map[string]bool{grant.Grantee.ID: true}
	for hop := 0; hop <= s.MaxDepth && len(frontier) > 0; hop++ {
		var next []string
		for _, agentID := range frontier {
			grants, err := s.Grants.ForAgent(ctx, tenantID, agentID)
			if err != nil {
				return fmt.Errorf("walk delegations of %q: %w", agentID, err)
			}
			for _, downstream := range grants {
				if downstream.Grantor.ID != agentID || seen[downstream.Grantee.ID] {
					continue
				}
				if downstream.Grantee.ID == grant.Grantor.ID {
					return fmt.Errorf("%w: agent %q already delegates back to %q", domain.ErrConflict, grant.Grantee.ID, grant.Grantor.ID)
				}
				seen[downstream.Grantee.ID] = true
				next = append(next, downstream.Grantee.ID)
			}
		}
		frontier = next
	}
	return nil
}

func (s *StudioDelegations) grantID() (string, error) {
	entropy := s.Entropy
	if entropy == nil {
		entropy = rand.Reader
	}
	raw := make([]byte, 12)
	if _, err := io.ReadFull(entropy, raw); err != nil {
		return "", fmt.Errorf("generate grant id: %w", err)
	}
	return "dg-" + hex.EncodeToString(raw), nil
}

func (s *StudioDelegations) now() time.Time {
	return clk.Now(s.Now).UTC()
}
