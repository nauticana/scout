package controlplane

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var delegationNow = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

type studioAuthorizerFunc func(context.Context, domain.StudioActor, domain.StudioAction, string) error

func (function studioAuthorizerFunc) Authorize(ctx context.Context, actor domain.StudioActor, action domain.StudioAction, agentID string) error {
	return function(ctx, actor, action, agentID)
}

func delegations(existing []domain.DelegationGrant, put *domain.DelegationGrant) *StudioDelegations {
	return &StudioDelegations{
		Authorizer: studioAuthorizerFunc(func(context.Context, domain.StudioActor, domain.StudioAction, string) error { return nil }),
		Grants: &fake.DelegationGrantRepository{
			ForAgentFunc: func(_ context.Context, _ int64, agentID string) ([]domain.DelegationGrant, error) {
				var held []domain.DelegationGrant
				for _, grant := range existing {
					if grant.Grantor.ID == agentID || grant.Grantee.ID == agentID {
						held = append(held, grant)
					}
				}
				return held, nil
			},
			PutFunc: func(_ context.Context, grant domain.DelegationGrant) error { *put = grant; return nil },
		},
		MaxDepth: 2, Entropy: bytes.NewReader(make([]byte, 12)), Now: func() time.Time { return delegationNow },
	}
}

func agentGrant(grantor, grantee string) domain.DelegationGrant {
	return domain.DelegationGrant{
		Grantor: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: grantor}, Grantee: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: grantee},
		ActionScope: "draft.*", ValidTo: delegationNow.Add(24 * time.Hour),
	}
}

func TestDelegationGrantIsBoundedAndTenantScoped(t *testing.T) {
	var put domain.DelegationGrant
	granted, err := delegations(nil, &put).Grant(context.Background(), humanActor(8, 9), agentGrant("planner", "writer"))
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if put.TenantID != 8 || put.GrantID != "dg-000000000000000000000000" || granted.ValidFrom != delegationNow || put.Grantee.Kind != domain.PrincipalAgent {
		t.Fatalf("put = %+v", put)
	}
	for name, change := range map[string]func(*domain.DelegationGrant){
		"self":     func(grant *domain.DelegationGrant) { grant.Grantee.ID = grant.Grantor.ID },
		"open end": func(grant *domain.DelegationGrant) { grant.ValidTo = time.Time{} },
		"too deep": func(grant *domain.DelegationGrant) { grant.MaxDepth = 3 },
		"no scope": func(grant *domain.DelegationGrant) { grant.ActionScope = " " },
		"currency": func(grant *domain.DelegationGrant) { grant.BudgetMinorUnits = 500 },
		"in past":  func(grant *domain.DelegationGrant) { grant.ValidTo = delegationNow.Add(-time.Hour) },
	} {
		grant := agentGrant("planner", "writer")
		change(&grant)
		if _, err := delegations(nil, &put).Grant(context.Background(), humanActor(8, 9), grant); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: error = %v, want validation", name, err)
		}
	}
}

func TestDelegationGrantRefusesACycle(t *testing.T) {
	var put domain.DelegationGrant
	existing := []domain.DelegationGrant{agentGrant("writer", "editor"), agentGrant("editor", "planner")}
	if _, err := delegations(existing, &put).Grant(context.Background(), humanActor(8, 9), agentGrant("planner", "writer")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if put.GrantID != "" {
		t.Fatal("a cyclic grant must not be stored")
	}
}

func TestDelegationRevokeOnlyTheAgentsOwnGrant(t *testing.T) {
	service := delegations(nil, new(domain.DelegationGrant))
	revoked := false
	repository := service.Grants.(*fake.DelegationGrantRepository)
	repository.GetFunc = func(context.Context, int64, string) (domain.DelegationGrant, error) {
		return agentGrant("planner", "writer"), nil
	}
	repository.RevokeFunc = func(context.Context, int64, string, string) error { revoked = true; return nil }
	if err := service.Revoke(context.Background(), humanActor(8, 9), "writer", "dg-1"); !errors.Is(err, domain.ErrNotFound) || revoked {
		t.Fatalf("grantee revoke = %v, revoked = %v", err, revoked)
	}
	if err := service.Revoke(context.Background(), humanActor(8, 9), "planner", "dg-1"); err != nil || !revoked {
		t.Fatalf("grantor revoke = %v, revoked = %v", err, revoked)
	}
}

func TestDelegationGrantNeedsSightOfTheGrantee(t *testing.T) {
	var put domain.DelegationGrant
	service := delegations(nil, &put)
	service.Authorizer = studioAuthorizerFunc(func(_ context.Context, _ domain.StudioActor, _ domain.StudioAction, agentID string) error {
		if agentID == "writer" {
			return domain.ErrForbidden
		}
		return nil
	})
	if _, err := service.Grant(context.Background(), humanActor(8, 9), agentGrant("planner", "writer")); !errors.Is(err, domain.ErrForbidden) || put.GrantID != "" {
		t.Fatalf("error = %v, put = %+v", err, put)
	}
}
