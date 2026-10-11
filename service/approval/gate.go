package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/scope"
)

// Gate is the ToolApprovalGate backed by durable requests. An ask opens its request and returns
// pending; asking again returns the recorded verdict, so a replayed turn resumes on the decision.
type Gate struct {
	Store contract.ApprovalStore
	// Notifier is optional; without it the inbox is pull-only. Leave it unset when
	// the store notifies through the outbox, or the approver is told twice.
	Notifier contract.Notifier
	// Deadline is how long a reviewer has before escalation applies; zero leaves the request open.
	Deadline time.Duration
	// Router is optional; when set it assigns each new request's approver, and a request it cannot
	// route is refused. Without it a request is routed by scope to whoever holds DECIDE there.
	Router contract.ApproverRouter
	// Archive holds edited proposals' revised arguments; without it an edited call cannot run.
	Archive contract.EvidenceArchive
	// Authorizer is optional; when set, an editor's verdict approves the revision it proposed
	// if the editor may decide that revision too. Without it every revision awaits its own verdict.
	Authorizer contract.ApprovalAuthorizer
	// Audit is optional; when set an editor's approval of its revision becomes evidence.
	Audit contract.AuditSink
	// Class labels requests this gate opens; the risk tier comes from each ask.
	Class domain.OutputClass
	// MaxAsksPerCall bounds the approval requests one tool call can owe; zero takes
	// DefaultMaxAsksPerCall. Lowering it leaves a call's later recorded requests unread.
	MaxAsksPerCall int
	Now            func() time.Time
}

// maxRuleIDLength matches turn_approval_request.rule_id.
const maxRuleIDLength = 80

// DefaultMaxAsksPerCall is the approval requests one tool call can owe by default.
const DefaultMaxAsksPerCall = 8

func (g *Gate) maxAsks() (int, error) {
	switch {
	case g.MaxAsksPerCall < 0:
		return 0, fmt.Errorf("approval gate: max asks per call cannot be negative")
	case g.MaxAsksPerCall == 0:
		return DefaultMaxAsksPerCall, nil
	}
	return g.MaxAsksPerCall, nil
}

// Decide opens or reads the approval owed for one tool call. A call's requests form a short
// chain: an ask reads the first request it finds that it opened, or that an approved earlier
// ask covers, and otherwise opens the next. An approval covers a later ask only at the same or a
// higher risk tier under the same route, so no ask is ever decided by a weaker authority. A
// request still awaiting its verdict holds the call. The same ask never routes or notifies twice.
func (g *Gate) Decide(ctx context.Context, call domain.ToolCall, ask domain.ApprovalAsk) (domain.ApprovalDecision, error) {
	if g.Store == nil {
		return domain.ApprovalDenied, fmt.Errorf("approval gate: store is required")
	}
	maxAsks, err := g.maxAsks()
	if err != nil {
		return domain.ApprovalDenied, err
	}
	if err := validateAsk(&ask); err != nil {
		return domain.ApprovalDenied, err
	}
	request := domain.ApprovalRequest{
		TenantID: call.TenantContext.TenantID, RequestID: call.RequestID, ConversationID: call.ConversationID,
		ScopeID: call.Principal.ScopeID, RuleID: ask.RuleID,
		Principal: domain.PrincipalRef{Kind: call.Principal.Kind, ID: call.Principal.ID},
		Action:    "tool:" + call.ToolID, Resource: call.ToolID,
		Class: g.class(), RiskTier: ask.RiskTier, Summary: "tool call " + call.ToolID + " under rule " + ask.RuleID,
		ProposedDigest: ProposalDigest(call), Requester: call.OnBehalfOf, Source: ask.Source,
	}
	if call.Principal.Kind == domain.PrincipalAgent {
		request.ReleaseVersion = call.Principal.Release
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
	for index := 0; index < maxAsks; index++ {
		request.ExecutionStepID = stepIDAt(call, index)
		key := domain.ApprovalKey{TenantID: request.TenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID}
		stored, err := g.Store.Get(ctx, key)
		if errors.Is(err, domain.ErrNotFound) {
			return g.open(ctx, call, request, edit)
		}
		if err != nil {
			return domain.ApprovalDenied, err
		}
		if stored.ProposedDigest != request.ProposedDigest {
			return domain.ApprovalDenied, fmt.Errorf("%w: step %d already awaits a different proposal", domain.ErrConflict, key.ExecutionStepID)
		}
		if sameAsk(stored, request) || stored.Status != domain.ApprovalStatusApproved {
			return g.settle(ctx, call, stored, edit)
		}
		covered, err := g.covers(ctx, stored, request)
		if err != nil {
			return domain.ApprovalDenied, err
		}
		if covered {
			return domain.ApprovalApproved, nil
		}
	}
	return domain.ApprovalDenied, fmt.Errorf("%w: tool call %q owes more than %d approvals", domain.ErrConflict, call.ToolID, maxAsks)
}

// open routes and opens the request for an ask nothing recorded covers.
func (g *Gate) open(ctx context.Context, call domain.ToolCall, request domain.ApprovalRequest, edit *domain.ProposalRevision) (domain.ApprovalDecision, error) {
	if err := g.route(ctx, &request, edit); err != nil {
		return domain.ApprovalDenied, err
	}
	stored, err := g.Store.Open(ctx, request)
	if err != nil {
		return domain.ApprovalDenied, err
	}
	decision, err := g.settle(ctx, call, stored, edit)
	if err == nil && decision == domain.ApprovalPending {
		g.notify(ctx, stored)
	}
	return decision, err
}

// sameAsk reports whether stored was opened for this ask at no lower risk.
func sameAsk(stored, request domain.ApprovalRequest) bool {
	return stored.RuleID == request.RuleID && stored.Source == request.Source &&
		scope.ApprovalRiskRank(request.RiskTier) <= scope.ApprovalRiskRank(stored.RiskTier)
}

// covers reports whether an approval given for another ask of the same call also decides this
// one: its risk tier is at least this ask's, and this ask would be routed under the same route,
// or for an unnamed route, to the same approver. Without a router every ask is decided by scope.
func (g *Gate) covers(ctx context.Context, stored, request domain.ApprovalRequest) (bool, error) {
	if scope.ApprovalRiskRank(request.RiskTier) > scope.ApprovalRiskRank(stored.RiskTier) {
		return false, nil
	}
	if g.Router == nil {
		return stored.Route.RouteID == "", nil
	}
	assignment, err := g.Router.Route(ctx, request, stored.Approver)
	if err != nil {
		return false, fmt.Errorf("route approval of %s: %w", request.Action, err)
	}
	if assignment.Authority.RouteID != "" || stored.Route.RouteID != "" {
		return assignment.Authority.RouteID == stored.Route.RouteID, nil
	}
	return assignment.Approver.ID != "" && assignment.Approver == stored.Approver, nil
}

// route assigns a new request's approver and deadline. A routed request may open vacant, which
// nobody can decide until escalation fills it; any other request without an approver is refused.
func (g *Gate) route(ctx context.Context, request *domain.ApprovalRequest, edit *domain.ProposalRevision) error {
	deadline := g.Deadline
	if g.Router != nil {
		var prefer domain.PrincipalRef
		if edit != nil {
			prefer = edit.Editor
		}
		assignment, err := g.Router.Route(ctx, *request, prefer)
		if err != nil {
			return fmt.Errorf("route approval of %s: %w", request.Action, err)
		}
		vacant := assignment.Approver.Kind == "" || assignment.Approver.ID == ""
		if vacant && assignment.Authority.RouteID == "" {
			return fmt.Errorf("%w: no approver for %s", domain.ErrForbidden, request.Action)
		}
		if !vacant {
			request.Approver = assignment.Approver
		}
		request.Route = assignment.Authority
		if assignment.Deadline > 0 {
			deadline = assignment.Deadline
		}
	}
	if deadline > 0 {
		request.DeadlineAt = g.now().Add(deadline)
	}
	return nil
}

// settle returns the recorded decision, first letting an editor approve the revision it proposed.
func (g *Gate) settle(ctx context.Context, call domain.ToolCall, stored domain.ApprovalRequest, edit *domain.ProposalRevision) (domain.ApprovalDecision, error) {
	if decision, decided, err := recorded(call, stored); decided {
		return decision, err
	}
	if edit != nil {
		var err error
		if stored, err = g.approveAsEditor(ctx, stored, *edit); err != nil {
			return domain.ApprovalDenied, err
		}
		if decision, decided, err := recorded(call, stored); decided {
			return decision, err
		}
	}
	return domain.ApprovalPending, nil
}

func validateAsk(ask *domain.ApprovalAsk) error {
	if !knownSource(ask.Source) {
		return fmt.Errorf("%w: unknown approval source %q", domain.ErrValidation, ask.Source)
	}
	if strings.TrimSpace(ask.RuleID) == "" || len(ask.RuleID) > maxRuleIDLength {
		return fmt.Errorf("%w: an approval ask names a rule of 1 to %d characters", domain.ErrValidation, maxRuleIDLength)
	}
	if ask.RiskTier == "" {
		ask.RiskTier = domain.DefaultApprovalRiskTier
	}
	if !validRisk(ask.RiskTier) {
		return fmt.Errorf("%w: unknown approval risk tier %q", domain.ErrValidation, ask.RiskTier)
	}
	return nil
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

// stepIDAt keys a call's index-th request within its turn. The tool call has no compiled step
// id, so the tool identity serves, with the idempotency key telling apart several calls of one
// tool in the same turn.
func stepIDAt(call domain.ToolCall, index int) int64 {
	identity := call.ToolID + "\x1f" + call.ToolVersion
	if call.IdempotencyKey != "" {
		identity += "\x1f" + call.IdempotencyKey
	}
	identity += fmt.Sprintf("\x1fask:%d", index)
	sum := sha256.Sum256([]byte(identity))
	var key int64
	for _, b := range sum[:7] {
		key = key<<8 | int64(b)
	}
	return key + 1
}

var _ contract.ToolApprovalGate = (*Gate)(nil)
