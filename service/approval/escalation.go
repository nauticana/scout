package approval

import (
	"context"
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

// Sweeper moves overdue requests on. It is composed into a keel worker; Scout
// starts no goroutines of its own.
type Sweeper struct {
	Store  *TableStore
	Policy contract.EscalationPolicy
	Audit  contract.AuditSink
	// Batch bounds one sweep; zero takes the store's page default.
	Batch int
	Now   func() time.Time
}

// Sweep escalates or expires every overdue request for one tenant and returns
// how many it moved.
func (s *Sweeper) Sweep(ctx context.Context, tenantID int64) (int, error) {
	if s.Store == nil || s.Policy == nil {
		return 0, fmt.Errorf("approval sweeper: store and escalation policy are required")
	}
	due, err := s.Store.DueBy(ctx, tenantID, domain.ApprovalFilter{TenantID: tenantID, DueBefore: s.now(), Limit: s.Batch})
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, request := range due {
		step, err := s.Policy.Escalate(ctx, request)
		if err != nil {
			return moved, err
		}
		if step.Backup.ID == "" {
			if err := s.expire(ctx, request, step.Reason); err != nil {
				return moved, err
			}
			moved++
			continue
		}
		if err := s.Store.Reassign(ctx, request, step); err != nil {
			return moved, err
		}
		if err := s.record(ctx, request, domain.ApprovalStatusEscalated, step.Reason, step.Backup); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

func (s *Sweeper) expire(ctx context.Context, request domain.ApprovalRequest, reason string) error {
	verdict := domain.ApprovalVerdict{
		RequestKey: domain.ApprovalKey{TenantID: request.TenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID},
		Status:     domain.ApprovalStatusExpired, Decider: platformPrincipal,
		ProposedDigest: request.ProposedDigest, Reason: reason, DecidedAt: s.now(),
	}
	if _, err := s.Store.Resolve(ctx, verdict); err != nil {
		return err
	}
	return s.record(ctx, request, domain.ApprovalStatusExpired, reason, domain.PrincipalRef{})
}

func (s *Sweeper) record(ctx context.Context, request domain.ApprovalRequest, status domain.ApprovalStatus, reason string, backup domain.PrincipalRef) error {
	if s.Audit == nil {
		return nil
	}
	resource := request.Resource
	if backup.ID != "" {
		resource = backup.ID
	}
	return s.Audit.Record(ctx, domain.DecisionRecord{
		TenantID: request.TenantID, Principal: platformPrincipal, ScopeID: request.ScopeID,
		Category: domain.DecisionCategoryApproval, Action: string(status), Resource: resource,
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
