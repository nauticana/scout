package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestRestrictedResolverAddsStandingDenials(t *testing.T) {
	_, store := newRestrictions(t)
	ctx := context.Background()
	release := fake.PolicyResolverFunc(func(context.Context, domain.Principal) (domain.PolicySet, error) {
		return domain.PolicySet{PolicyID: "writer", Version: "release-1", Statements: []domain.PolicyStatement{
			{ID: "pay", Effect: domain.PolicyAllow, Actions: []string{"tool.invoke"}, Resources: []string{"payments.refund"}},
		}}, nil
	})
	evaluator := &SetEvaluator{Policies: &RestrictedResolver{Release: release, Restrictions: store}}
	subject := domain.DecisionSubject{Principal: domain.Principal{Kind: domain.PrincipalAgent, ID: "writer", TenantID: 7, Release: "1"}, Action: "tool.invoke", Resource: "payments.refund"}
	if decision, err := evaluator.Decide(ctx, subject); err != nil || decision.Outcome != domain.DecisionAllow || decision.PolicyVersion != "release-1" {
		t.Fatalf("no layer: %+v, %v", decision, err)
	}
	if _, err := store.ReplaceTenant(ctx, platformService, 7, domain.RestrictionLayer{Denials: []domain.PolicyStatement{denial("no-payments")}}, ""); err != nil {
		t.Fatalf("ReplaceTenant: %v", err)
	}
	decision, err := evaluator.Decide(ctx, subject)
	if err != nil || decision.Outcome != domain.DecisionDeny || len(decision.PolicyVersion) != 64 {
		t.Fatalf("a tenant denial must win over the release allow: %+v, %v", decision, err)
	}
}

type unreadableLayers struct{}

func (unreadableLayers) Layers(context.Context, int64) (domain.RestrictionLayers, error) {
	return domain.RestrictionLayers{}, errors.New("store down")
}

func TestRestrictedResolverFailsClosed(t *testing.T) {
	release := fake.PolicyResolverFunc(func(context.Context, domain.Principal) (domain.PolicySet, error) {
		return domain.PolicySet{Statements: []domain.PolicyStatement{{ID: "all", Effect: domain.PolicyAllow, Actions: []string{"*"}, Resources: []string{"*"}}}}, nil
	})
	evaluator := &SetEvaluator{Policies: &RestrictedResolver{Release: release, Restrictions: unreadableLayers{}}}
	decision, err := evaluator.Decide(context.Background(), domain.DecisionSubject{
		Principal: domain.Principal{Kind: domain.PrincipalAgent, ID: "writer", TenantID: 7}, Action: "tool.invoke", Resource: "payments.refund"})
	if err == nil || decision.Outcome != domain.DecisionDeny {
		t.Fatalf("an unreadable layer must deny: %+v, %v", decision, err)
	}
	if _, err = (&RestrictedResolver{Release: release}).Policies(context.Background(), domain.Principal{}); err == nil {
		t.Fatal("a resolver without restriction layers must refuse")
	}
}
