package principal

import (
	"errors"
	"testing"
	"time"

	"github.com/nauticana/charter/sdk/authority"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/domain"
)

func catalogExponent(currency string) (int, error) {
	if currency == "USD" {
		return 2, nil
	}
	return 0, errors.New("not in the currency catalog")
}

func TestCharterChain_IsAcceptedByDelegationPolicy(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	chain := domain.AuthorityChain{
		{GrantID: "g2", Grantor: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "agent-1"}, MaxDepth: 0,
			BudgetMinorUnits: 5000, Currency: "USD", NotBefore: from},
		{GrantID: "g1", Grantor: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}, MaxDepth: 1,
			ApprovalRequired: true, NotBefore: from, NotAfter: from.AddDate(1, 0, 0)},
	}

	converted, err := CharterChain(chain, "acme", catalogExponent)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	// An open-ended Scout hop must not become a nil validity — Charter fails closed on nil.
	if converted[0].Validity == nil || converted[0].Validity.To != "" {
		t.Fatalf("open-ended hop validity = %+v", converted[0].Validity)
	}
	if converted[0].Delegator.Kind != model.KindAgentIdentity || converted[1].Delegator.Kind != model.KindHumanIdentity {
		t.Fatalf("delegator kinds = %s, %s", converted[0].Delegator.Kind, converted[1].Delegator.Kind)
	}
	limit := converted[0].Limits[0]
	if limit.Currency != "USD" || *limit.CurrencyExponent != 2 || limit.Operator != "lte" {
		t.Fatalf("budget limit = %+v", limit)
	}

	policy := authority.DelegationPolicy{}
	at := from.AddDate(0, 6, 0)
	measures := map[string]authority.Measure{BudgetLimitKind: {Value: int64(4999), Currency: "USD", CurrencyExponent: 2}}
	if reason := policy.Check(converted, at, measures); reason != "" {
		t.Fatalf("Charter rejected a well-formed chain: %s", reason)
	}
	if !policy.ApprovalRequired(converted) {
		t.Fatal("approval required on hop 1 was lost")
	}
	measures[BudgetLimitKind] = authority.Measure{Value: int64(5001), Currency: "USD", CurrencyExponent: 2}
	if reason := policy.Check(converted, at, measures); reason == "" {
		t.Fatal("an over-budget measure must be refused")
	}
}

func TestCharterChain_FailsClosed(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := map[string]domain.AuthorityChain{
		"service grantor":     {{GrantID: "g", Grantor: domain.PrincipalRef{Kind: domain.PrincipalService, ID: "svc"}, NotBefore: from}},
		"malformed currency":  {{GrantID: "g", Grantor: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "1"}, BudgetMinorUnits: 1, Currency: "usd", NotBefore: from}},
		"unassigned currency": {{GrantID: "g", Grantor: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "1"}, BudgetMinorUnits: 1, Currency: "ZZZ", NotBefore: from}},
	}
	for name, chain := range cases {
		if _, err := CharterChain(chain, "acme", catalogExponent); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	budgeted := domain.AuthorityChain{{GrantID: "g", Grantor: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "1"}, BudgetMinorUnits: 1, Currency: "USD", NotBefore: from}}
	if _, err := CharterChain(budgeted, "acme", nil); err == nil {
		t.Error("a budgeted hop without an exponent lookup must fail")
	}
	if out, err := CharterChain(nil, "acme", nil); err != nil || out != nil {
		t.Errorf("empty chain = %v, %v", out, err)
	}
}
