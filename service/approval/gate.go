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
	// Notifier is optional; without it the inbox is pull-only. Leave it unset when
	// the store notifies through the outbox, or the approver is told twice.
	Notifier contract.Notifier
	// Deadline is how long a reviewer has before escalation applies; zero leaves the request open.
	Deadline time.Duration
	// Router is optional; when set it names each request's approver, and a request it cannot
	// route is refused. Without it a request is routed by scope to whoever holds DECIDE there.
	Router contract.ApproverRouter
	// Archive holds edited proposals' revised arguments; without it an edited call cannot run.
	Archive contract.EvidenceArchive
	// Authorizer is optional; when set, an editor's verdict approves the revision it proposed
	// if the editor may decide that revision too. Without it every revision awaits its own verdict.
	Authorizer contract.ApprovalAuthorizer
	// Audit is optional; when set an editor's approval of its revision becomes evidence.
	Audit contract.AuditSink
	// RiskTier and Class label requests this gate opens.
	RiskTier domain.RiskTier
	Class    domain.OutputClass
	Now      func() time.Time
}

// Decide opens or reads the approval owed for one irreversible tool call. A call that
// revises an edited proposal opens its own request, linked to the original.
func (g *Gate) Decide(ctx context.Context, call domain.ToolCall, ruleID string) (domain.ApprovalDecision, error) {
	if g.Store == nil {
		return domain.ApprovalDenied, fmt.Errorf("approval gate: store is required")
	}
	request := domain.ApprovalRequest{
		TenantID: call.TenantContext.TenantID, RequestID: call.RequestID, ConversationID: call.ConversationID,
		ExecutionStepID: stepIDFor(call), ScopeID: call.Principal.ScopeID, RuleID: ruleID,
		Principal: domain.PrincipalRef{Kind: call.Principal.Kind, ID: call.Principal.ID},
		Action:    "tool:" + call.ToolID, Resource: call.ToolID,
		Class: g.class(), RiskTier: g.riskTier(), Summary: "tool call " + call.ToolID + " under rule " + ruleID,
		ProposedDigest: ProposalDigest(call),
	}
	var edit *domain.ProposalRevision
	if call.RevisesProposal != 0 {
		revision, err := g.edit(ctx, call)
		if err != nil {
			return domain.ApprovalDenied, err
		}
		edit = &revision
		request.RevisesStepID, request.Evidence = call.RevisesProposal, revision.Arguments
		request.Summary = "revised " + request.Summary
	}
	if g.Deadline > 0 {
		request.DeadlineAt = g.now().Add(g.Deadline)
	}
	if g.Router != nil {
		approver, err := g.Router.Route(ctx, request)
		if err != nil {
			return domain.ApprovalDenied, fmt.Errorf("route approval of %s: %w", request.Action, err)
		}
		if approver.Kind == "" || approver.ID == "" {
			return domain.ApprovalDenied, fmt.Errorf("%w: no approver for %s", domain.ErrForbidden, request.Action)
		}
		request.Approver = approver
	}
	stored, err := g.Store.Open(ctx, request)
	if err != nil {
		return domain.ApprovalDenied, err
	}
	if decision, decided, err := recorded(call, stored); decided {
		return decision, err
	}
	if edit != nil {
		if stored, err = g.approveAsEditor(ctx, stored, *edit); err != nil {
			return domain.ApprovalDenied, err
		}
		if decision, decided, err := recorded(call, stored); decided {
			return decision, err
		}
	}
	g.notify(ctx, stored)
	return domain.ApprovalPending, nil
}

// recorded maps a terminal verdict to the gate's decision. An edited proposal never runs
// as proposed, only its revision does, so asking to run the original fails the turn.
func recorded(call domain.ToolCall, request domain.ApprovalRequest) (domain.ApprovalDecision, bool, error) {
	switch request.Status {
	case domain.ApprovalStatusApproved:
		return domain.ApprovalApproved, true, nil
	case domain.ApprovalStatusEdited:
		return domain.ApprovalDenied, true, fmt.Errorf("%w: a decider edited tool call %q", domain.ErrProposalRevised, call.ToolID)
	case domain.ApprovalStatusRejected, domain.ApprovalStatusExpired, domain.ApprovalStatusWithdrawn:
		return domain.ApprovalDenied, true, nil
	default:
		return domain.ApprovalPending, false, nil
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
// id, so the tool identity serves, with the idempotency key telling apart several
// calls of one tool in the same turn.
func stepIDFor(call domain.ToolCall) int64 {
	identity := call.ToolID + "\x1f" + call.ToolVersion
	if call.IdempotencyKey != "" {
		identity += "\x1f" + call.IdempotencyKey
	}
	sum := sha256.Sum256([]byte(identity))
	var key int64
	for _, b := range sum[:7] {
		key = key<<8 | int64(b)
	}
	return key + 1
}

var _ contract.ToolApprovalGate = (*Gate)(nil)
