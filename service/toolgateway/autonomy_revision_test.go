package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// A decider's revision is governed like any proposal: validated, authorized, and decided again.
func TestRevisedCallPassesTheWholeGovernanceChainAgain(t *testing.T) {
	sent := 0
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyExecuteWithApproval, false, domain.ApprovalApproved, &sent, &asked)
	gateway.Registry = &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: "https://example.invalid/tool",
			InputSchema: []byte(`{"type":"object","required":["qty"]}`)}, nil
	}}
	var decided []domain.ToolCall
	gateway.Approvals = fake.ToolApprovalGateFunc(func(_ context.Context, call domain.ToolCall, ask domain.ApprovalAsk) (domain.ApprovalDecision, error) {
		decided = append(decided, call)
		return domain.ApprovalApproved, nil
	})
	revised := validToolCall()
	revised.Arguments, revised.RevisesProposal, revised.IdempotencyKey = []byte(`{"note":"no qty"}`), 77, "revision:1"
	if _, err := gateway.Invoke(context.Background(), revised); !errors.Is(err, domain.ErrValidation) || sent != 0 || len(decided) != 0 {
		t.Fatalf("an invalid revision: %v, sent %d, decided %d", err, sent, len(decided))
	}
	revised.Arguments = []byte(`{"qty":5}`)
	if _, err := gateway.Invoke(context.Background(), revised); err != nil || sent != 1 {
		t.Fatalf("a valid revision: %v, sent %d", err, sent)
	}
	if len(decided) != 1 || decided[0].RevisesProposal != 77 || string(decided[0].Arguments) != `{"qty":5}` {
		t.Fatalf("the gate decided %+v, want the revision with its link", decided)
	}
}

func TestEditedOriginalIsRefusedWithItsCause(t *testing.T) {
	sent := 0
	var asked []domain.ApprovalAsk
	gateway := autonomyGateway(domain.AutonomyExecuteWithApproval, false, "", &sent, &asked)
	gateway.Approvals = fake.ToolApprovalGateFunc(func(context.Context, domain.ToolCall, domain.ApprovalAsk) (domain.ApprovalDecision, error) {
		return domain.ApprovalDenied, fmt.Errorf("gate: %w", domain.ErrProposalRevised)
	})
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrProposalRevised) || sent != 0 {
		t.Fatalf("edited original: %v, sent %d", err, sent)
	}
}
