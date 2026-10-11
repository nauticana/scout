package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func requireApproval(params string) domain.Obligation {
	obligation := domain.Obligation{Kind: domain.ObligationRequireApproval}
	if params != "" {
		obligation.Params = json.RawMessage(params)
	}
	return obligation
}

func TestPolicyApprovalAsksOnceAtTheRiskiestTier(t *testing.T) {
	sent := 0
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyBounded, false, domain.ApprovalApproved, &sent, &asked)
	gateway.Policy = allowingPolicy(requireApproval(`{"risk_tier":"medium"}`), requireApproval(`{"risk_tier":"critical"}`), requireApproval(""))
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err != nil || sent != 1 {
		t.Fatalf("error = %v, sent = %d", err, sent)
	}
	want := domain.ApprovalAsk{Source: domain.ApprovalSourcePolicy, RuleID: "policy", RiskTier: domain.RiskCritical}
	if len(asked) != 1 || asked[0] != want {
		t.Fatalf("asked = %+v, want one policy ask at critical", asked)
	}
}

func TestAPendingAskHoldsTheCallBeforeAnyOther(t *testing.T) {
	sent := 0
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyExecuteWithApproval, false, domain.ApprovalPending, &sent, &asked)
	gateway.Policy = allowingPolicy(requireApproval(""))
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrApprovalPending) || sent != 0 {
		t.Fatalf("error = %v, sent = %d", err, sent)
	}
	if len(asked) != 1 || asked[0].Source != domain.ApprovalSourcePolicy || asked[0].RiskTier != domain.RiskHigh {
		t.Fatalf("asked = %+v, want the policy ask alone at an equal tier", asked)
	}
}

func TestALowerTierPolicyAskNeverDisplacesTheOperatingModesAsk(t *testing.T) {
	sent := 0
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyExecuteWithApproval, false, domain.ApprovalApproved, &sent, &asked)
	gateway.Policy = allowingPolicy(requireApproval(`{"risk_tier":"low"}`))
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err != nil || sent != 1 {
		t.Fatalf("error = %v, sent = %d", err, sent)
	}
	want := []domain.ApprovalAsk{
		{Source: domain.ApprovalSourceAutonomy, RuleID: "autonomy", RiskTier: domain.RiskHigh},
		{Source: domain.ApprovalSourcePolicy, RuleID: "policy", RiskTier: domain.RiskLow},
	}
	if len(asked) != 2 || asked[0] != want[0] || asked[1] != want[1] {
		t.Fatalf("asked = %+v, want the mode's high ask first, then the policy's", asked)
	}
}

func TestEveryAskMustBeApprovedBeforeTheCallRuns(t *testing.T) {
	sent := 0
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyExecuteWithApproval, false, "", &sent, &asked)
	gateway.Policy = allowingPolicy(requireApproval(`{"risk_tier":"critical"}`))
	gateway.Approvals = fake.ToolApprovalGateFunc(func(_ context.Context, _ domain.ToolCall, ask domain.ApprovalAsk) (domain.ApprovalDecision, error) {
		asked = append(asked, ask)
		if ask.Source == domain.ApprovalSourceAutonomy {
			return domain.ApprovalDenied, nil
		}
		return domain.ApprovalApproved, nil
	})
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrForbidden) || sent != 0 {
		t.Fatalf("error = %v, sent = %d", err, sent)
	}
	if len(asked) != 2 || asked[0].Source != domain.ApprovalSourcePolicy {
		t.Fatalf("asked = %+v, want the critical policy ask first", asked)
	}
}

func TestMalformedApprovalParamsFailBeforeTheGate(t *testing.T) {
	for _, params := range []string{`{"risk_tier":"extreme"}`, `{"approver":"cfo"}`, `[]`} {
		sent := 0
		var asked []domain.ApprovalAsk
		gateway := autonomyGateway(domain.AutonomyBounded, false, domain.ApprovalApproved, &sent, &asked)
		gateway.Policy = allowingPolicy(requireApproval(params))
		if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrValidation) || len(asked) != 0 || sent != 0 {
			t.Fatalf("%s: error = %v, asked = %d, sent = %d", params, err, len(asked), sent)
		}
	}
}

func TestRequireApprovalWithoutAGateUsesAnEnforcerOrDegrades(t *testing.T) {
	sent := 0
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyBounded, false, "", &sent, &asked)
	gateway.Approvals = nil
	gateway.Policy = allowingPolicy(requireApproval(""))
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrDegraded) || sent != 0 {
		t.Fatalf("no gate, no enforcer: %v, sent %d", err, sent)
	}
	enforced := 0
	gateway.Obligations = []contract.ObligationEnforcer{&fake.ObligationEnforcer{
		ObligationKind: domain.ObligationRequireApproval,
		EnforceFunc:    func(context.Context, domain.DecisionSubject, domain.Obligation) error { enforced++; return nil },
	}}
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err != nil || enforced != 1 || sent != 1 {
		t.Fatalf("a product enforcer: %v, enforced %d, sent %d", err, enforced, sent)
	}
	gateway.Approvals = fake.ToolApprovalGateFunc(func(context.Context, domain.ToolCall, domain.ApprovalAsk) (domain.ApprovalDecision, error) {
		return domain.ApprovalApproved, nil
	})
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err == nil || sent != 1 {
		t.Fatalf("a gate and an enforcer of require_approval are ambiguous: %v, sent %d", err, sent)
	}
}

func TestAForbiddenModeNeverOpensAnApproval(t *testing.T) {
	sent := 0
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyDraft, false, domain.ApprovalApproved, &sent, &asked)
	gateway.Policy = allowingPolicy(requireApproval(""))
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrForbidden) || len(asked) != 0 || sent != 0 {
		t.Fatalf("error = %v, asked = %d, sent = %d", err, len(asked), sent)
	}
}
