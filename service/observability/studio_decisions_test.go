package observability

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestStudioDecisionsReadOnlyTheActorsTenantUnderTheAuditGrant(t *testing.T) {
	actor := domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "41"}}
	var asked []string
	var queried domain.DecisionQuery
	decisions := &StudioDecisions{
		Audit: fake.AuditQueryFunc(func(_ context.Context, query domain.DecisionQuery) (domain.DecisionPage, error) {
			queried = query
			return domain.DecisionPage{NextBefore: 9}, nil
		}),
		Principals: fake.PrincipalResolverFunc(func(_ context.Context, tenantID int64, ref domain.PrincipalRef) (domain.Principal, error) {
			return domain.Principal{Kind: ref.Kind, ID: ref.ID, TenantID: tenantID}, nil
		}),
		Roles: fake.PrincipalAuthorizerFunc(func(_ context.Context, principal domain.Principal, object, action, value string) (domain.AuthorizationGrant, error) {
			asked = []string{object, action, value}
			return domain.AuthorizationGrant{Allowed: principal.ID == "41"}, nil
		}),
	}
	page, err := decisions.Decisions(context.Background(), actor, domain.DecisionQuery{TenantID: 99, Category: domain.DecisionCategoryToolInvoke, Limit: 5})
	if err != nil || page.NextBefore != 9 {
		t.Fatalf("Decisions = %+v, %v", page, err)
	}
	if queried.TenantID != 7 || queried.Category != domain.DecisionCategoryToolInvoke || queried.Limit != 5 {
		t.Fatalf("query = %+v, want the actor's tenant", queried)
	}
	if asked[0] != "AGENT_AUDIT" || asked[1] != "VIEW" || asked[2] != "tenant" {
		t.Fatalf("authorized %v", asked)
	}

	other := actor
	other.Principal.ID = "42"
	for name, tc := range map[string]struct {
		decisions *StudioDecisions
		actor     domain.StudioActor
		query     domain.DecisionQuery
		want      error
	}{
		"no grant":        {decisions, other, domain.DecisionQuery{}, domain.ErrForbidden},
		"not composed":    {&StudioDecisions{}, actor, domain.DecisionQuery{}, domain.ErrNotReady},
		"no tenant":       {decisions, domain.StudioActor{Principal: actor.Principal}, domain.DecisionQuery{}, domain.ErrValidation},
		"an agent":        {decisions, domain.StudioActor{TenantID: 7, Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "a"}}, domain.DecisionQuery{}, domain.ErrValidation},
		"negative limit":  {decisions, actor, domain.DecisionQuery{Limit: -1}, domain.ErrValidation},
		"negative before": {decisions, actor, domain.DecisionQuery{Before: -1}, domain.ErrValidation},
		"unknown outcome": {decisions, actor, domain.DecisionQuery{Outcome: "?"}, domain.ErrValidation},
	} {
		if _, err := tc.decisions.Decisions(context.Background(), tc.actor, tc.query); !errors.Is(err, tc.want) {
			t.Fatalf("%s: error = %v, want %v", name, err, tc.want)
		}
	}
}
