package principal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

// A stored grant reads back as written: the select lists the inserted columns in order, and the
// grantee is always an agent.
func TestTableGrantsRoundTripsAGrant(t *testing.T) {
	query := &authQueryFake{}
	grants := &TableGrants{DB: authDBFake{query: query}}
	ctx := context.Background()
	from, to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	grant := domain.DelegationGrant{TenantID: 7, GrantID: "G-1",
		Grantor: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}, Grantee: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "writer-a"},
		ActionScope: "draft.*", MaxDepth: 2, BudgetMinorUnits: 500, Currency: "EUR", ApprovalRequired: true, ValidFrom: from, ValidTo: to}
	if err := grants.Put(ctx, grant); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if query.asked != qGrantPut || len(query.args) != 14 || query.args[5] != "writer-a" {
		t.Fatalf("put %s args = %v", query.asked, query.args)
	}
	query.rows = [][]any{append(append([]any(nil), query.args[1:13]...), nil)}
	held, err := grants.ForGrantee(ctx, 7, grant.Grantee)
	if err != nil || len(held) != 1 || held[0] != grant {
		t.Fatalf("ForGrantee = %+v, %v", held, err)
	}
	if query.asked != qGrantsGrantee || len(query.args) != 2 || query.args[0] != int64(7) || query.args[1] != "writer-a" {
		t.Fatalf("grantee %s args = %v", query.asked, query.args)
	}
	query.asked = ""
	if held, err = grants.ForGrantee(ctx, 7, domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "writer-a"}); err != nil || len(held) != 0 || query.asked != "" {
		t.Fatalf("a human grantee holds %+v, %v, asked %q", held, err, query.asked)
	}
	grant.Grantee.Kind = domain.PrincipalHuman
	if err = grants.Put(ctx, grant); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a human grantee: want ErrValidation, got %v", err)
	}
}
