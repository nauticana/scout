package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Gate is the ToolApprovalGate backed by durable requests. The first call opens
// the request and returns pending; later calls return the recorded verdict, so a
// replayed turn resumes on the decision instead of asking again.
type Gate struct {
	Store contract.ApprovalStore
	// Notifier is optional; without it the inbox is pull-only.
	Notifier contract.Notifier
	// Deadline is how long a reviewer has before escalation applies; zero leaves the request open.
	Deadline time.Duration
	// RiskTier and Class label requests this gate opens.
	RiskTier domain.RiskTier
	Class    domain.OutputClass
	Now      func() time.Time
}

// Decide opens or reads the approval owed for one irreversible tool call.
func (g *Gate) Decide(ctx context.Context, call domain.ToolCall, ruleID string) (domain.ApprovalDecision, error) {
	if g.Store == nil {
		return domain.ApprovalDenied, fmt.Errorf("approval gate: store is required")
	}
	request := domain.ApprovalRequest{
		TenantID: call.TenantContext.TenantID, RequestID: call.RequestID, ConversationID: call.ConversationID,
		ExecutionStepID: stepIDFor(call), ScopeID: call.Principal.ScopeID, RuleID: ruleID,
		Principal: domain.PrincipalRef{Kind: call.Principal.Kind, ID: call.Principal.ID},
		Action:    "tool:" + call.ToolID, Resource: call.ToolID,
		Class: g.class(), RiskTier: g.riskTier(), Summary: "irreversible tool call " + call.ToolID,
		ProposedDigest: ProposalDigest(call),
	}
	if g.Deadline > 0 {
		request.DeadlineAt = g.now().Add(g.Deadline)
	}
	stored, err := g.Store.Open(ctx, request)
	if err != nil {
		return domain.ApprovalDenied, err
	}
	switch stored.Status {
	case domain.ApprovalStatusApproved:
		return domain.ApprovalApproved, nil
	case domain.ApprovalStatusRejected, domain.ApprovalStatusEdited, domain.ApprovalStatusExpired, domain.ApprovalStatusWithdrawn:
		return domain.ApprovalDenied, nil
	default:
		g.notify(ctx, stored)
		return domain.ApprovalPending, nil
	}
}

func (g *Gate) notify(ctx context.Context, request domain.ApprovalRequest) {
	if g.Notifier == nil || request.Approver.ID == "" {
		return
	}
	// A notification failure must not deny an otherwise valid pending request;
	// the inbox remains the authoritative queue.
	_ = g.Notifier.Notify(ctx, domain.Notification{
		TenantID: request.TenantID, Recipient: request.Approver,
		Subject: request.Action, Reference: request.RequestID,
		RiskTier: request.RiskTier, DueAt: request.DeadlineAt,
	})
}

func (g *Gate) class() domain.OutputClass {
	if g.Class != "" {
		return g.Class
	}
	return domain.OutputApprovalRequest
}

func (g *Gate) riskTier() domain.RiskTier {
	if g.RiskTier != "" {
		return g.RiskTier
	}
	return domain.RiskHigh
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// ProposalDigest binds a verdict to the exact call. Changing any argument
// produces a different digest, so an approval can never be reused for a
// different action.
func ProposalDigest(call domain.ToolCall) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "%d\x1f%s\x1f%s\x1f%s\x1f", call.TenantContext.TenantID, call.RequestID, call.ToolID, call.ToolVersion)
	sum.Write(call.Arguments)
	return hex.EncodeToString(sum.Sum(nil))
}

// stepIDFor keys the request within its turn. The tool call has no compiled step
// id, so the tool identity within the request serves as the stable key.
func stepIDFor(call domain.ToolCall) int64 {
	sum := sha256.Sum256([]byte(call.ToolID + "\x1f" + call.ToolVersion))
	var key int64
	for _, b := range sum[:7] {
		key = key<<8 | int64(b)
	}
	return key + 1
}

var _ contract.ToolApprovalGate = (*Gate)(nil)
