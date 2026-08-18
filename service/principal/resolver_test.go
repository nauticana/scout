package principal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func hop(grantID string, depth int, notAfter time.Time) domain.AuthorityHop {
	return domain.AuthorityHop{
		GrantID: grantID, Grantor: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"},
		MaxDepth: depth, NotAfter: notAfter,
	}
}

func agent(chain ...domain.AuthorityHop) domain.Principal {
	return domain.Principal{Kind: domain.PrincipalAgent, ID: "ap-agent", TenantID: 7, Authority: chain}
}

func verify(principal domain.Principal) (domain.AuthorityRef, error) {
	grantee := domain.PrincipalRef{Kind: principal.Kind, ID: principal.ID}
	grants := make(stubGrants, 0, len(principal.Authority))
	for _, authority := range principal.Authority {
		grants = append(grants, domain.DelegationGrant{
			TenantID: principal.TenantID, GrantID: authority.GrantID, Grantor: authority.Grantor, Grantee: grantee,
			MaxDepth: authority.MaxDepth, BudgetMinorUnits: authority.BudgetMinorUnits, Currency: authority.Currency,
			ApprovalRequired: authority.ApprovalRequired, ValidFrom: authority.NotBefore, ValidTo: authority.NotAfter,
		})
		grantee = authority.Grantor
	}
	verifier := &ChainVerifier{Grants: grants, MaxDepth: 2, Now: func() time.Time { return time.Unix(1700000000, 0).UTC() }}
	return verifier.Verify(context.Background(), principal)
}

func TestVerifyAcceptsAnUndelegatedPrincipal(t *testing.T) {
	ref, err := verify(agent())
	if err != nil || ref.Subject.ID != "ap-agent" || ref.GrantID != "" {
		t.Fatalf("ref = %+v, error = %v", ref, err)
	}
}

func TestVerifyReturnsTheImmediateGrant(t *testing.T) {
	ref, err := verify(agent(hop("g1", 1, time.Time{})))
	if err != nil {
		t.Fatal(err)
	}
	if ref.GrantID != "g1" || ref.Grantor.ID != "42" {
		t.Fatalf("ref = %+v, want the immediate grant recorded", ref)
	}
}

func TestVerifyRejectsAnExpiredGrant(t *testing.T) {
	expired := time.Unix(1600000000, 0).UTC()
	_, err := verify(agent(hop("g1", 1, expired)))
	if !errors.Is(err, domain.ErrGrantExpired) {
		t.Fatalf("error = %v, want an expired grant", err)
	}
}

func TestVerifyRejectsAChainDeeperThanTheLimit(t *testing.T) {
	_, err := verify(agent(
		hop("g1", 3, time.Time{}), hop("g2", 3, time.Time{}), hop("g3", 3, time.Time{})))
	if !errors.Is(err, domain.ErrDelegationDepth) {
		t.Fatalf("error = %v, want the configured depth limit to bind", err)
	}
}

func TestVerifyRejectsAHopThatOutspentItsGrantedDepth(t *testing.T) {
	_, err := verify(agent(hop("g1", 0, time.Time{}), hop("g2", 1, time.Time{})))
	if !errors.Is(err, domain.ErrDelegationDepth) {
		t.Fatalf("error = %v, want the grant's own depth to bind", err)
	}
}

func TestVerifyRejectsAZeroPrincipal(t *testing.T) {
	if _, err := verify(domain.Principal{}); !errors.Is(err, domain.ErrPrincipalUnknown) {
		t.Fatalf("error = %v, want the zero principal rejected", err)
	}
}
