package approval

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// BackupEscalation routes an overdue request to a configured backup and then
// abandons it. A backup needs its own grant: escalation moves who decides, never
// what may be decided.
type BackupEscalation struct {
	// Backups maps a risk tier to the principal that receives its overdue work.
	Backups map[domain.RiskTier]domain.PrincipalRef
	// Extension is the deadline the backup receives; zero leaves it open.
	Extension  time.Duration
	Authorizer contract.ApprovalAuthorizer
}

// Escalate returns the step for one overdue request; an empty backup abandons it.
func (e *BackupEscalation) Escalate(ctx context.Context, request domain.ApprovalRequest) (domain.EscalationStep, error) {
	backup, found := e.Backups[request.RiskTier]
	if !found {
		return domain.EscalationStep{Reason: "no backup configured for " + string(request.RiskTier)}, nil
	}
	if backup.ID == request.Approver.ID && backup.Kind == request.Approver.Kind {
		return domain.EscalationStep{Reason: "backup is the current approver"}, nil
	}
	if e.Authorizer == nil {
		return domain.EscalationStep{}, fmt.Errorf("backup escalation: approval authorizer is required")
	}
	candidate := request
	candidate.Approver = backup
	if err := e.Authorizer.AuthorizeApproval(ctx, candidate, backup); err != nil {
		return domain.EscalationStep{}, err
	}
	return domain.EscalationStep{Backup: backup, Extension: e.Extension, Reason: "deadline passed"}, nil
}

// Sweeper moves overdue requests on and fills vacant routed ones. It is composed into a keel
// worker; Scout starts no goroutines of its own.
type Sweeper struct {
	Store  *TableStore
	Policy contract.EscalationPolicy
	// Records fails the turn parked on a request that expires.
	Records contract.TurnRecordStore
	Audit   contract.AuditSink
	// Notifier tells a request's new approver; leave it unset when the store notifies through the outbox.
	Notifier contract.Notifier
	// Batch bounds each of the overdue, vacant, and unsettled pages one sweep reads; zero takes the store's page default.
	Batch int
	Now   func() time.Time
}

// Sweep escalates, holds, or expires every overdue request for one tenant, fills vacant routed
// requests, then fails the turns still parked on expired requests, and returns how many requests
// it moved. A request another sweep or a verdict moved first is skipped; any other failure is
// collected and the sweep continues.
func (s *Sweeper) Sweep(ctx context.Context, tenantID int64) (int, error) {
	if s.Store == nil || s.Policy == nil || s.Records == nil {
		return 0, fmt.Errorf("approval sweeper: store, escalation policy, and turn records are required")
	}
	if s.Notifier != nil && s.Store.NotifyThroughOutbox {
		return 0, fmt.Errorf("approval sweeper: a notifier and the store's outbox would tell each approver twice")
	}
	due, err := s.Store.DueBy(ctx, tenantID, domain.ApprovalFilter{TenantID: tenantID, DueBefore: s.now(), Limit: s.Batch})
	if err != nil {
		return 0, err
	}
	vacant, err := s.Store.Vacant(ctx, tenantID, s.Batch)
	if err != nil {
		return 0, err
	}
	moved := 0
	var errs []error
	for _, request := range append(due, vacant...) {
		changed, err := s.move(ctx, request)
		if changed {
			moved++
		}
		if err != nil && !errors.Is(err, domain.ErrConflict) {
			errs = append(errs, fmt.Errorf("approval %d: %w", request.ID, err))
		}
	}
	unsettled, err := s.Store.Unsettled(ctx, tenantID, s.Batch)
	if err != nil {
		return moved, errors.Join(append(errs, err)...)
	}
	for _, request := range unsettled {
		if err := s.settleTurn(ctx, request); err != nil {
			errs = append(errs, fmt.Errorf("approval %d: %w", request.ID, err))
		}
	}
	return moved, errors.Join(errs...)
}

// move applies one escalation step and reports whether the request changed; a failure after the
// change, such as its audit or notice, is returned with it.
func (s *Sweeper) move(ctx context.Context, request domain.ApprovalRequest) (bool, error) {
	step, err := s.Policy.Escalate(ctx, request)
	if err != nil {
		return false, err
	}
	switch action := step.Resolved(); action {
	case domain.EscalationWait:
		return false, nil
	case domain.EscalationExpire:
		return s.expire(ctx, request, step.Reason)
	case domain.EscalationReassign, domain.EscalationVacate, domain.EscalationHold:
		if err := s.Store.Reassign(ctx, request, step); err != nil {
			return false, err
		}
		recorded := string(domain.ApprovalStatusEscalated)
		if action == domain.EscalationHold {
			recorded = auditHeld
		}
		return true, errors.Join(s.notify(ctx, request, step), s.record(ctx, request, recorded, auditReason(step), step.Backup))
	default:
		return false, fmt.Errorf("%w: unknown escalation action %q", domain.ErrValidation, action)
	}
}

// expire closes the request as it was read, then fails the turn parked on it. A turn left parked
// by a failure here stays unsettled, and a later sweep fails it.
func (s *Sweeper) expire(ctx context.Context, request domain.ApprovalRequest, reason string) (bool, error) {
	key := domain.ApprovalKey{TenantID: request.TenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID}
	verdict := domain.ApprovalVerdict{
		RequestKey: key, Status: domain.ApprovalStatusExpired, Decider: platformPrincipal,
		ProposedDigest: request.ProposedDigest, Reason: reason, DecidedAt: s.now(), Assignment: fenceOf(request),
	}
	if _, err := s.Store.Resolve(ctx, verdict); err != nil {
		return false, err
	}
	return true, errors.Join(s.settleTurn(ctx, request), s.record(ctx, request, string(domain.ApprovalStatusExpired), reason, domain.PrincipalRef{}))
}

// settleTurn fails the turn parked on an expired request and marks the request settled. A turn
// that is gone or already ended another way has nothing parked, so it is settled as it is.
func (s *Sweeper) settleTurn(ctx context.Context, request domain.ApprovalRequest) error {
	key := domain.ApprovalKey{TenantID: request.TenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID}
	err := failTurn(ctx, s.Records, key, domain.ApprovalStatusExpired)
	if err != nil && !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return s.Store.TurnSettled(ctx, request.TenantID, request.ID)
}

// notify tells the approver a move assigned; the store's outbox does it instead when enabled.
func (s *Sweeper) notify(ctx context.Context, request domain.ApprovalRequest, step domain.EscalationStep) error {
	if s.Notifier == nil || step.Backup.ID == "" || step.Backup == request.Approver {
		return nil
	}
	due := request.DeadlineAt
	if !step.KeepDeadline {
		due = time.Time{}
		if step.Extension > 0 {
			due = s.now().Add(step.Extension)
		}
	}
	if err := s.Notifier.Notify(ctx, domain.Notification{
		TenantID: request.TenantID, Recipient: step.Backup, Subject: request.Action,
		Reference: request.RequestID, RiskTier: request.RiskTier, DueAt: due,
	}); err != nil {
		return fmt.Errorf("notify approver %s: %w", step.Backup.ID, err)
	}
	return nil
}

// auditReason names the route, the role or position, and the step a routed move lands on.
func auditReason(step domain.EscalationStep) string {
	if step.Route.RouteID == "" {
		return step.Reason
	}
	return fmt.Sprintf("%s (route %s, %s %s, step %d)", step.Reason, step.Route.RouteID,
		step.Route.Reference.Kind, step.Route.Reference.Ref, step.Route.Step)
}

// auditHeld is the audit action of a request held open after its escalation is exhausted.
const auditHeld = "held"

func (s *Sweeper) record(ctx context.Context, request domain.ApprovalRequest, action, reason string, backup domain.PrincipalRef) error {
	if s.Audit == nil {
		return nil
	}
	resource := request.Resource
	if backup.ID != "" {
		resource = backup.ID
	}
	return s.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: request.TenantID, Principal: platformPrincipal, ScopeID: request.ScopeID,
		Category: domain.DecisionCategoryApproval, Action: action, Resource: resource,
		Outcome: domain.DecisionDeny, Reason: reason,
		RequestID: request.RequestID, ConversationID: request.ConversationID, OccurredAt: s.now(),
	})
}

func (s *Sweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// platformPrincipal attributes an expiry or escalation the platform performed.
var platformPrincipal = domain.PrincipalRef{Kind: domain.PrincipalService, ID: "platform"}

var _ contract.EscalationPolicy = (*BackupEscalation)(nil)
