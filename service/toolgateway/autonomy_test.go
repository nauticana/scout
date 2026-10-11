package toolgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func autonomyGateway(mode domain.AutonomyMode, readOnly bool, decision domain.ApprovalDecision, sent *int, asked *[]domain.ApprovalAsk) *GovernedGateway {
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		*sent++
		return domain.ToolResult{Output: []byte(`{}`)}, nil
	}))
	gateway.Registry = &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: "https://example.invalid/tool", ReadOnly: readOnly}, nil
	}}
	gateway.Limits = &fake.PrincipalLimits{AutonomyModeFunc: func(context.Context, domain.Principal, time.Time) (domain.AutonomyMode, error) {
		return mode, nil
	}}
	gateway.Approvals = fake.ToolApprovalGateFunc(func(_ context.Context, _ domain.ToolCall, ask domain.ApprovalAsk) (domain.ApprovalDecision, error) {
		*asked = append(*asked, ask)
		return decision, nil
	})
	return gateway
}

func TestOperatingModeDecidesWhatAnAgentMayCall(t *testing.T) {
	autonomyAsk := domain.ApprovalAsk{Source: domain.ApprovalSourceAutonomy, RuleID: "autonomy", RiskTier: domain.RiskHigh}
	for _, test := range []struct {
		mode     domain.AutonomyMode
		readOnly bool
		decision domain.ApprovalDecision
		want     error
		asks     bool
	}{
		{domain.AutonomyHumanOnly, true, "", domain.ErrForbidden, false},
		{domain.AutonomyAdvise, true, "", nil, false},
		{domain.AutonomyDraft, false, "", domain.ErrForbidden, false},
		{domain.AutonomyExecuteWithApproval, true, "", nil, false},
		{domain.AutonomyExecuteWithApproval, false, domain.ApprovalPending, domain.ErrApprovalPending, true},
		{domain.AutonomyExecuteWithApproval, false, domain.ApprovalDenied, domain.ErrForbidden, true},
		{domain.AutonomyExecuteWithApproval, false, domain.ApprovalApproved, nil, true},
		{domain.AutonomyBounded, false, "", nil, false},
	} {
		sent := 0
		var asked []domain.ApprovalAsk
		_, err := autonomyGateway(test.mode, test.readOnly, test.decision, &sent, &asked).Invoke(context.Background(), validToolCall())
		if test.want == nil && err != nil || test.want != nil && !errors.Is(err, test.want) {
			t.Fatalf("%s read-only=%t: error = %v, want %v", test.mode, test.readOnly, err, test.want)
		}
		if (sent == 1) != (test.want == nil) || (len(asked) == 1) != test.asks || test.asks && asked[0] != autonomyAsk {
			t.Fatalf("%s read-only=%t: sent = %d, asked = %v", test.mode, test.readOnly, sent, asked)
		}
	}
}

func TestOperatingModeNeedsAnApprovalGate(t *testing.T) {
	sent := 0
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyExecuteWithApproval, false, "", &sent, &asked)
	gateway.Approvals = nil
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrDegraded) || sent != 0 {
		t.Fatalf("error = %v, sent = %d", err, sent)
	}
}
