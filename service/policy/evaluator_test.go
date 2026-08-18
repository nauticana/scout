package policy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

type stubResolver struct {
	set domain.PolicySet
	err error
}

func (s stubResolver) Policies(context.Context, domain.Principal) (domain.PolicySet, error) {
	return s.set, s.err
}

func agent() domain.Principal {
	return domain.Principal{Kind: domain.PrincipalAgent, ID: "ap-agent", TenantID: 7, Release: "3"}
}

func evaluator(statements ...domain.PolicyStatement) *SetEvaluator {
	return &SetEvaluator{
		Policies: stubResolver{set: domain.PolicySet{PolicyID: "p", Version: "1", Statements: statements}},
		Now:      func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

func allow(id string, obligations ...domain.Obligation) domain.PolicyStatement {
	return domain.PolicyStatement{
		ID: id, Effect: domain.PolicyAllow, Actions: []string{"tool:invoke"},
		Resources: []string{"search"}, Obligations: obligations,
	}
}

func subject(action, resource string) domain.DecisionSubject {
	return domain.DecisionSubject{Principal: agent(), Action: action, Resource: resource}
}

func TestDecideDeniesWhenNothingMatches(t *testing.T) {
	decision, err := evaluator(allow("a")).Decide(context.Background(), subject("tool:invoke", "delete"))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != domain.DecisionDeny {
		t.Fatalf("outcome = %q, want an unmatched resource denied", decision.Outcome)
	}
}

func TestDecideDeniesOnAnEmptyPolicySet(t *testing.T) {
	decision, err := evaluator().Decide(context.Background(), subject("tool:invoke", "search"))
	if err != nil || decision.Outcome != domain.DecisionDeny {
		t.Fatalf("outcome = %q, error = %v, want a closed default", decision.Outcome, err)
	}
}

func TestDecideLetsDenyBeatAnEarlierAllow(t *testing.T) {
	deny := domain.PolicyStatement{
		ID: "z", Effect: domain.PolicyDeny, Actions: []string{"*"}, Resources: []string{"search"},
	}
	decision, err := evaluator(allow("a"), deny).Decide(context.Background(), subject("tool:invoke", "search"))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != domain.DecisionDeny || decision.PolicyVersion != "1" {
		t.Fatalf("decision = %+v, want deny to win regardless of order", decision)
	}
}

func TestDecideCarriesObligationsOnAnAllow(t *testing.T) {
	decision, err := evaluator(allow("a", domain.Obligation{Kind: domain.ObligationRequireApproval})).
		Decide(context.Background(), subject("tool:invoke", "search"))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != domain.DecisionAllow || len(decision.Obligations) != 1 {
		t.Fatalf("decision = %+v, want one obligation", decision)
	}
}

func TestDecideCombinesObligationsFromEveryMatchingAllow(t *testing.T) {
	first := allow("a")
	second := allow("b", domain.Obligation{Kind: domain.ObligationRequireApproval})
	decision, err := evaluator(first, second).Decide(context.Background(), subject("tool:invoke", "search"))
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Obligations) != 1 || decision.Obligations[0].Kind != domain.ObligationRequireApproval {
		t.Fatalf("obligations = %+v, want the later matching allow to remain binding", decision.Obligations)
	}
}

func TestDecideMatchesOnlyExactOrTrailingWildcard(t *testing.T) {
	statement := domain.PolicyStatement{
		ID: "a", Effect: domain.PolicyAllow, Actions: []string{"tool:*"}, Resources: []string{"search-*"},
	}
	for name, test := range map[string]struct {
		action, resource string
		want             domain.DecisionOutcome
	}{
		"prefix":     {"tool:invoke", "search-web", domain.DecisionAllow},
		"other verb": {"model:invoke", "search-web", domain.DecisionDeny},
		"other kind": {"tool:invoke", "delete-all", domain.DecisionDeny},
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := evaluator(statement).Decide(context.Background(), subject(test.action, test.resource))
			if err != nil || decision.Outcome != test.want {
				t.Fatalf("outcome = %q, error = %v, want %q", decision.Outcome, err, test.want)
			}
		})
	}
}

func TestDecideAppliesConditions(t *testing.T) {
	statement := allow("a")
	statement.Conditions = []byte(`{"region":"eu"}`)
	sub := subject("tool:invoke", "search")
	sub.Environment = []byte(`{"region":"us"}`)
	decision, err := evaluator(statement).Decide(context.Background(), sub)
	if err != nil || decision.Outcome != domain.DecisionDeny {
		t.Fatalf("outcome = %q, error = %v, want an unmet condition denied", decision.Outcome, err)
	}
}

func TestDecideFailsClosedWhenPolicyIsUnavailable(t *testing.T) {
	broken := &SetEvaluator{Policies: stubResolver{err: errors.New("unreachable")}}
	decision, err := broken.Decide(context.Background(), subject("tool:invoke", "search"))
	if err == nil || decision.Outcome != domain.DecisionDeny {
		t.Fatalf("outcome = %q, error = %v, want a closed failure", decision.Outcome, err)
	}
}

func TestDecideRejectsAZeroPrincipal(t *testing.T) {
	decision, err := evaluator(allow("a")).Decide(context.Background(), domain.DecisionSubject{Action: "tool:invoke"})
	if !errors.Is(err, domain.ErrPrincipalUnknown) || decision.Outcome != domain.DecisionDeny {
		t.Fatalf("outcome = %q, error = %v", decision.Outcome, err)
	}
}
