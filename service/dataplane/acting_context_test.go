package dataplane

import (
	"errors"
	"reflect"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestActingContextSurvivesTheQueueAndNeverCrossesATenant(t *testing.T) {
	delegated := domain.TurnRequest{
		TenantContext: domain.TenantContext{TenantID: 7, PriorityClass: "interactive", ScopeID: "emea"},
		Principal: domain.Principal{Kind: domain.PrincipalAgent, ID: "specialist", TenantID: 7, Release: "v3",
			Authority: domain.AuthorityChain{{GrantID: "g1", Grantor: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "lead"}, MaxDepth: 1}}},
		OnBehalfOf:       domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"},
		DelegationBounds: domain.DelegationBounds{RemainingDepth: 1, BudgetMinorUnits: 500, Currency: "USD"},
		WorkItemID:       9, WorkItemDepth: 2,
	}
	encoded, err := encodeActingContext(delegated)
	if err != nil {
		t.Fatal(err)
	}
	claimed := domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 7}}
	if err = decodeActingContext(encoded.(string), &claimed); err != nil || !reflect.DeepEqual(claimed, delegated) {
		t.Fatalf("claimed = %+v, %v", claimed, err)
	}
	foreign := domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 8}}
	if err = decodeActingContext(encoded.(string), &foreign); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("want ErrForbidden for another tenant's row, got %v", err)
	}
	delegated.Principal.TenantID = 8
	if _, err = encodeActingContext(delegated); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("want ErrForbidden for a foreign principal, got %v", err)
	}
	if encoded, err = encodeActingContext(domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 7}}); encoded != nil || err != nil {
		t.Fatalf("a turn naming no one stores nothing: %v, %v", encoded, err)
	}
}
