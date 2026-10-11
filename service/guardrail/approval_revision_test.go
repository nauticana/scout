package guardrail

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// An approval rule decides a revised call as the revision it is, and an edited original ends the turn.
func TestApprovalRuleDecidesTheRevisionNotTheOriginal(t *testing.T) {
	var seen domain.ToolCall
	refuse := false
	gate := fake.ToolApprovalGateFunc(func(_ context.Context, call domain.ToolCall, _ domain.ApprovalAsk) (domain.ApprovalDecision, error) {
		seen = call
		if refuse {
			return domain.ApprovalDenied, fmt.Errorf("gate: %w", domain.ErrProposalRevised)
		}
		return domain.ApprovalApproved, nil
	})
	h := newHarness(t, EnforcerConfig{Approvals: gate})
	config := rules(t, rule("release.approve", domain.GuardrailKindIrreversibleToolApproval, domain.GuardrailActionBlock, `{"tools":["wire"]}`))
	revised := toolCall("wire", `{"amount":5}`)
	revised.RevisesProposal = 77
	if _, err := h.enforcer.BeforeTool(context.Background(), config, revised); err != nil {
		t.Fatalf("approved revision: %v", err)
	}
	if seen.RevisesProposal != 77 || string(seen.Arguments) != `{"amount":5}` {
		t.Fatalf("the gate saw %+v, want the revision with its link", seen)
	}
	refuse = true
	if _, err := h.enforcer.BeforeTool(context.Background(), config, toolCall("wire", `{"amount":1000}`)); !errors.Is(err, domain.ErrProposalRevised) || IsPending(err) {
		t.Fatalf("edited original: %v", err)
	}
}
