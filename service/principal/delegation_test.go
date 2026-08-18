package principal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

type stubGrants []domain.DelegationGrant

func (s stubGrants) Put(context.Context, domain.DelegationGrant) error { return nil }

func (s stubGrants) Get(_ context.Context, _ int64, grantID string) (domain.DelegationGrant, error) {
	for _, grant := range s {
		if grant.GrantID == grantID {
			return grant, nil
		}
	}
	return domain.DelegationGrant{}, domain.ErrNotFound
}

func (s stubGrants) Revoke(context.Context, int64, string, string) error { return nil }

func (s stubGrants) ForGrantee(context.Context, int64, domain.PrincipalRef) ([]domain.DelegationGrant, error) {
	return s, nil
}

var (
	manager   = domain.Principal{Kind: domain.PrincipalHuman, ID: "42", TenantID: 7, ScopeID: "unit"}
	assistant = domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "assistant"}
)

func grant(scope string, depth int, validTo time.Time) domain.DelegationGrant {
	return domain.DelegationGrant{
		TenantID: 7, GrantID: "g1",
		Grantor: domain.PrincipalRef{Kind: manager.Kind, ID: manager.ID}, Grantee: assistant,
		ActionScope: scope, MaxDepth: depth, ValidTo: validTo,
	}
}

func authorizer(held bool, grants ...domain.DelegationGrant) *GrantAuthorizer {
	return &GrantAuthorizer{
		Grants: stubGrants(grants),
		Authorizer: fake.PrincipalAuthorizerFunc(func(context.Context, domain.Principal, string, string, string) (domain.AuthorizationGrant, error) {
			return domain.AuthorizationGrant{Allowed: held}, nil
		}),
		AuthorizationObject: "AGENT_ACTION",
		Now:                 func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func TestAuthorizeReturnsTheGrantBounds(t *testing.T) {
	authorization, err := authorizer(true, grant("invoice:*", 2, time.Time{})).
		Authorize(context.Background(), manager, assistant, "invoice:approve")
	if err != nil {
		t.Fatal(err)
	}
	if authorization.GrantID != "g1" || authorization.Bounds.RemainingDepth != 2 || authorization.Bounds.ScopeID != "unit" {
		t.Fatalf("authorization = %+v", authorization)
	}
}

func TestAuthorizeRefusesWhenTheGrantorNoLongerHoldsTheAction(t *testing.T) {
	// A revoked role must not keep flowing through an older grant.
	_, err := authorizer(false, grant("invoice:*", 2, time.Time{})).
		Authorize(context.Background(), manager, assistant, "invoice:approve")
	if !errors.Is(err, domain.ErrAuthorityExceeded) {
		t.Fatalf("error = %v, want the grantor's lost authority to bind", err)
	}
}

func TestAuthorizeIgnoresAnExpiredOrUncoveredGrant(t *testing.T) {
	expired := grant("invoice:*", 2, time.Unix(1600000000, 0).UTC())
	if _, err := authorizer(true, expired).Authorize(context.Background(), manager, assistant, "invoice:approve"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("expired: error = %v", err)
	}
	if _, err := authorizer(true, grant("invoice:*", 2, time.Time{})).Authorize(context.Background(), manager, assistant, "payment:send"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("uncovered action: error = %v", err)
	}
}

func TestNarrowShrinksEveryBoundAndNeverRegainsOne(t *testing.T) {
	parent := domain.DelegationBounds{RemainingDepth: 3, BudgetMinorUnits: 1000, Currency: "EUR", ScopeID: "unit"}

	next, err := Narrow(parent, domain.DelegationBounds{RemainingDepth: 9, BudgetMinorUnits: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if next.RemainingDepth != 2 || next.BudgetMinorUnits != 1000 {
		t.Fatalf("bounds = %+v, want a hop that cannot regain depth or budget", next)
	}

	tighter, err := Narrow(parent, domain.DelegationBounds{RemainingDepth: 1, BudgetMinorUnits: 400})
	if err != nil {
		t.Fatal(err)
	}
	if tighter.RemainingDepth != 1 || tighter.BudgetMinorUnits != 400 {
		t.Fatalf("bounds = %+v, want the tighter child values kept", tighter)
	}
}

func TestNarrowStopsAtZeroDepth(t *testing.T) {
	_, err := Narrow(domain.DelegationBounds{RemainingDepth: 0}, domain.DelegationBounds{RemainingDepth: 5})
	if !errors.Is(err, domain.ErrDelegationDepth) {
		t.Fatalf("error = %v, want an exhausted chain refused", err)
	}
}

func TestNarrowPreservesAChildGrantThatForbidsFurtherDelegation(t *testing.T) {
	next, err := Narrow(domain.DelegationBounds{RemainingDepth: 3}, domain.DelegationBounds{RemainingDepth: 0})
	if err != nil {
		t.Fatal(err)
	}
	if next.RemainingDepth != 0 {
		t.Fatalf("remaining depth = %d, want the child's zero-depth grant to bind", next.RemainingDepth)
	}
}

func TestNarrowRefusesAScopeOrCurrencyChange(t *testing.T) {
	parent := domain.DelegationBounds{RemainingDepth: 2, ScopeID: "unit", Currency: "EUR", BudgetMinorUnits: 100}
	if _, err := Narrow(parent, domain.DelegationBounds{ScopeID: "other-unit"}); !errors.Is(err, domain.ErrAuthorityExceeded) {
		t.Fatalf("error = %v, want a scope move refused", err)
	}
	if _, err := Narrow(parent, domain.DelegationBounds{Currency: "USD", BudgetMinorUnits: 10}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want a currency change refused", err)
	}
}
