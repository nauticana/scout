package approval

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/nauticana/keel/actiontoken"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
)

// Resumer is the reviewer's side of human-in-the-loop: it lists what a decider may
// decide, records a verdict, and returns an approved turn to the queue or fails a
// refused one. Recording a verdict without requeueing would leave the turn parked.
type Resumer struct {
	Store      contract.ApprovalStore
	Inbox      contract.ApprovalInbox
	Authorizer contract.ApprovalAuthorizer
	Records    contract.TurnRecordStore
	Queue      contract.TurnRequeuer
	// Audit is optional; when set every verdict becomes evidence.
	Audit contract.AuditSink
	// Links mints and redeems single-use decision links for email and chat; nil offers none.
	Links *actiontoken.Service
	// LinkMaxRisk is the riskiest tier a link may decide; riskier requests, and those of an
	// authenticated-only route, are decided only in an authenticated view. Empty allows no link.
	LinkMaxRisk domain.RiskTier
	// Archive keeps an edited verdict's revised arguments where evidence lives; nil refuses edits.
	Archive contract.EvidenceArchive
	// MaxRevisedArgumentBytes bounds revised arguments; zero takes DefaultMaxRevisedArgumentBytes.
	MaxRevisedArgumentBytes int
	Now                     func() time.Time
}

var _ contract.ApprovalDesk = (*Resumer)(nil)

var errSpentLink = fmt.Errorf("%w: the decision link is spent, expired, or not for this decision", domain.ErrNotFound)

var riskRank = map[domain.RiskTier]int{domain.RiskLow: 1, domain.RiskMedium: 2, domain.RiskHigh: 3, domain.RiskCritical: 4}

// Pending returns the open requests decider may decide now, soonest deadline first.
func (r *Resumer) Pending(ctx context.Context, tenantID int64, decider domain.PrincipalRef, limit int) ([]domain.ApprovalRequest, error) {
	if r.Inbox == nil || r.Authorizer == nil {
		return nil, fmt.Errorf("approval resumer: inbox and authorizer are required")
	}
	requests, err := r.Inbox.Pending(ctx, domain.ApprovalFilter{TenantID: tenantID, Approver: decider, Limit: limit})
	if err != nil {
		return nil, err
	}
	decidable := requests[:0]
	for _, request := range requests {
		err := r.Authorizer.AuthorizeApproval(ctx, request, decider)
		if errors.Is(err, domain.ErrForbidden) {
			continue
		}
		if err != nil {
			return nil, err
		}
		decidable = append(decidable, request)
	}
	return decidable, nil
}

// Resolve records an approval, rejection, or edit once and moves the parked turn. An
// approved turn is requeued and a rejected one fails, since the work must not continue
// unapproved. An edit closes the proposal with revised arguments and requeues the turn,
// which proposes the revision anew under the same governance and runs it in its place.
func (r *Resumer) Resolve(ctx context.Context, verdict domain.ApprovalVerdict) (domain.ApprovalRequest, error) {
	if r.Store == nil || r.Authorizer == nil || r.Records == nil || r.Queue == nil {
		return domain.ApprovalRequest{}, fmt.Errorf("approval resumer: store, authorizer, turn records, and queue are required")
	}
	switch verdict.Status {
	case domain.ApprovalStatusApproved, domain.ApprovalStatusRejected:
		if len(verdict.RevisedArguments) > 0 || verdict.Revision.URI != "" {
			return domain.ApprovalRequest{}, fmt.Errorf("%w: only an edited verdict carries revised arguments", domain.ErrValidation)
		}
	case domain.ApprovalStatusEdited:
	default:
		return domain.ApprovalRequest{}, fmt.Errorf("%w: a decider approves, rejects, or edits; %q is not a verdict", domain.ErrValidation, verdict.Status)
	}
	request, err := r.Store.Get(ctx, verdict.RequestKey)
	if err != nil {
		return domain.ApprovalRequest{}, err
	}
	if err := awaitsApprover(request); err != nil {
		return domain.ApprovalRequest{}, err
	}
	if err := r.Authorizer.AuthorizeApproval(ctx, request, verdict.Decider); err != nil {
		return domain.ApprovalRequest{}, err
	}
	verdict.Assignment = fenceOf(request)
	if verdict.Authority.Subject.ID == "" {
		verdict.Authority.Subject = verdict.Decider
	}
	if verdict.Status == domain.ApprovalStatusEdited {
		if verdict, err = r.archiveRevision(ctx, request, verdict); err != nil {
			return domain.ApprovalRequest{}, err
		}
	}
	request, err = r.Store.Resolve(ctx, verdict)
	if err != nil {
		return domain.ApprovalRequest{}, err
	}
	if err := recordVerdict(ctx, r.Audit, request, verdict, r.now()); err != nil {
		return request, err
	}
	key := verdict.RequestKey
	if verdict.Status == domain.ApprovalStatusRejected {
		return request, failTurn(ctx, r.Records, key, verdict.Status)
	}
	err = r.Records.Resume(ctx, key.TenantID, key.RequestID)
	// A held turn keeps the verdict for whoever releases it; nothing queues it now.
	if errors.Is(err, domain.ErrTurnHeld) {
		return request, nil
	}
	if err != nil {
		return request, err
	}
	if err := r.Queue.Requeue(ctx, key.TenantID, key.RequestID); err != nil {
		return request, fmt.Errorf("requeue resumed turn %q: %w", key.RequestID, err)
	}
	return request, nil
}

// failTurn fails the turn parked on a refused or expired request; a replay that finds the turn
// already failed with the same code succeeds, and a held turn keeps the verdict for whoever
// releases it.
func failTurn(ctx context.Context, records contract.TurnRecordStore, key domain.ApprovalKey, status domain.ApprovalStatus) error {
	err := records.FailParked(ctx, key.TenantID, key.RequestID, string(status))
	if errors.Is(err, domain.ErrTurnHeld) {
		return nil
	}
	if !errors.Is(err, domain.ErrConflict) {
		return err
	}
	record, findErr := records.Find(ctx, key.TenantID, key.RequestID)
	if findErr == nil && record.Status == domain.TurnFailed && string(record.Payload) == string(status) {
		return nil
	}
	return errors.Join(err, findErr)
}

// awaitsApprover refuses a routed request nobody is assigned yet: it waits for escalation to
// fill it and never falls back to whoever holds DECIDE.
func awaitsApprover(request domain.ApprovalRequest) error {
	if request.Route.RouteID != "" && request.Approver.ID == "" {
		return fmt.Errorf("%w: approval %d awaits an eligible approver", domain.ErrForbidden, request.ID)
	}
	return nil
}

// fenceOf is the assignment a verdict on request is decided against.
func fenceOf(request domain.ApprovalRequest) domain.ApprovalFence {
	return domain.ApprovalFence{Step: request.Route.Step, Approver: request.Approver}
}

// MintLink returns a single-use token by which one person decides one request with one
// verdict, for the product's notifier to deliver. It lapses at the request's deadline;
// domain.ErrAuthenticatedView tells the notifier to link the authenticated view instead.
func (r *Resumer) MintLink(ctx context.Context, key domain.ApprovalKey, userID int64, status domain.ApprovalStatus, ttl time.Duration) (string, error) {
	request, binding, err := r.linkBinding(ctx, key, status)
	if err != nil {
		return "", err
	}
	if r.Authorizer == nil {
		return "", fmt.Errorf("approval resumer: authorizer is required")
	}
	if err := r.Authorizer.AuthorizeApproval(ctx, request, humanRef(userID)); err != nil {
		return "", err
	}
	if !request.DeadlineAt.IsZero() {
		ttl = min(ttl, request.DeadlineAt.Sub(r.now()))
	}
	return r.Links.Mint(ctx, keelport.UserRef{UserID: userID, PartnerID: key.TenantID}, binding, ttl)
}

// RedeemLink decides the request with the verdict a link was minted for, authorizing its
// person afresh. A spent, expired, or mismatched link is ErrNotFound.
func (r *Resumer) RedeemLink(ctx context.Context, token string, key domain.ApprovalKey, status domain.ApprovalStatus, reason string) (domain.ApprovalRequest, error) {
	request, binding, err := r.linkBinding(ctx, key, status)
	if errors.Is(err, domain.ErrNotReady) {
		return domain.ApprovalRequest{}, err
	}
	if err != nil {
		// Redemption needs no session, so nothing about the request shows before its token does.
		return domain.ApprovalRequest{}, errSpentLink
	}
	run := func(ctx context.Context, claim actiontoken.Claim) ([]byte, error) {
		if claim.User.PartnerID != key.TenantID {
			return nil, &actiontoken.Failure{Reason: "link belongs to another tenant"}
		}
		_, err := r.Resolve(ctx, domain.ApprovalVerdict{
			RequestKey: key, Status: status, Decider: humanRef(claim.User.UserID),
			ProposedDigest: request.ProposedDigest, Reason: reason, DecidedAt: r.now(),
		})
		if errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrConflict) {
			return nil, &actiontoken.Failure{Reason: err.Error()}
		}
		return []byte(status), err
	}
	_, err = r.Links.Redeem(ctx, token, binding, run)
	if unknown, ok := errors.AsType[*actiontoken.UnknownOutcome](err); ok {
		// Resolve replays to the stored verdict, so settling the claim at once is safe.
		_, err = r.Links.Reconcile(ctx, unknown.ClaimID, run)
	}
	var failure *actiontoken.Failure
	switch {
	case errors.Is(err, actiontoken.ErrInvalid):
		return domain.ApprovalRequest{}, errSpentLink
	case errors.Is(err, actiontoken.ErrInFlight):
		return domain.ApprovalRequest{}, fmt.Errorf("%w: the decision link is being redeemed", domain.ErrConflict)
	case errors.As(err, &failure):
		return domain.ApprovalRequest{}, fmt.Errorf("%w: %s", domain.ErrForbidden, failure.Reason)
	case err != nil:
		return domain.ApprovalRequest{}, err
	}
	return r.Store.Get(ctx, key)
}

// linkBinding ties a link to the request, its verdict, and the exact proposal it decides.
func (r *Resumer) linkBinding(ctx context.Context, key domain.ApprovalKey, status domain.ApprovalStatus) (domain.ApprovalRequest, actiontoken.Binding, error) {
	if r.Links == nil || r.Store == nil {
		return domain.ApprovalRequest{}, actiontoken.Binding{}, fmt.Errorf("%w: decision links are not composed", domain.ErrNotReady)
	}
	if status != domain.ApprovalStatusApproved && status != domain.ApprovalStatusRejected {
		return domain.ApprovalRequest{}, actiontoken.Binding{}, fmt.Errorf("%w: a link approves or rejects", domain.ErrValidation)
	}
	request, err := r.Store.Get(ctx, key)
	if err != nil {
		return domain.ApprovalRequest{}, actiontoken.Binding{}, err
	}
	if err := awaitsApprover(request); err != nil {
		return domain.ApprovalRequest{}, actiontoken.Binding{}, err
	}
	if limit, ok := riskRank[r.LinkMaxRisk]; !ok || riskRank[request.RiskTier] > limit || request.Route.AuthenticatedOnly {
		return domain.ApprovalRequest{}, actiontoken.Binding{}, fmt.Errorf("%w: route %q, %s risk", domain.ErrAuthenticatedView, request.Route.RouteID, request.RiskTier)
	}
	return request, actiontoken.Binding{
		Action:      "approval/" + string(status),
		ResourceKey: fmt.Sprintf("%d/%s/%d", key.TenantID, key.RequestID, key.ExecutionStepID),
		Digest:      request.ProposedDigest,
	}, nil
}

// recordVerdict makes a verdict evidence when an audit sink is composed; revised arguments
// stay in the archive and never enter the record.
func recordVerdict(ctx context.Context, audit contract.AuditSink, request domain.ApprovalRequest, verdict domain.ApprovalVerdict, at time.Time) error {
	if audit == nil {
		return nil
	}
	outcome := domain.DecisionDeny
	if verdict.Status == domain.ApprovalStatusApproved {
		outcome = domain.DecisionAllow
	}
	ctx = domain.WithDecisionScope(ctx, "approval/", request.RequestID, "/", request.ExecutionStepID)
	return audit.Record(ctx, domain.DecisionRecord{
		TenantID: request.TenantID, Principal: verdict.Decider, Authority: verdict.Authority,
		ScopeID: request.ScopeID, Category: domain.DecisionCategoryApproval, Action: string(verdict.Status),
		Resource: request.Resource, Outcome: outcome, Reason: verdict.Reason,
		RequestID: request.RequestID, ConversationID: request.ConversationID, OccurredAt: at,
	})
}

func (r *Resumer) now() time.Time {
	return clk.Now(r.Now)
}

func humanRef(userID int64) domain.PrincipalRef {
	return domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: strconv.FormatInt(userID, 10)}
}
