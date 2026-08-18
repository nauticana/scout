// Package approval is the durable human-in-the-loop path: a governed action that
// needs a person parks the turn, records what is owed, and resumes on the verdict.
// Nothing here decides who may approve — that is the policy decision point.
package approval

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qApprovalOpen         = "scout_approval_open"
	qApprovalGet          = "scout_approval_get"
	qApprovalResolve      = "scout_approval_resolve"
	qApprovalDecision     = "scout_approval_decision"
	qApprovalDecisionGet  = "scout_approval_decision_get"
	qApprovalPending      = "scout_approval_pending"
	qApprovalPendingScope = "scout_approval_pending_scope"
	qApprovalDue          = "scout_approval_due"
	qApprovalEscalate     = "scout_approval_escalate"

	approvalColumns = `id, request_id, conversation_id, execution_step_id, principal_kind, principal_id,
       approver_kind, approver_id, scope_id, rule_id, requested_action, resource_ref, output_class_code,
       risk_tier_code, summary, evidence_uri, evidence_digest, proposed_digest, status_code,
       deadline_at, created_at, resolved_at`
)

var approvalQueries = map[string]string{
	qApprovalOpen: `
INSERT INTO approval_request
       (id, tenant_id, request_id, conversation_id, execution_step_id, principal_kind, principal_id,
        approver_kind, approver_id, scope_id, rule_id, requested_action, resource_ref, output_class_code,
        risk_tier_code, summary, evidence_uri, evidence_digest, proposed_digest, status_code, deadline_at)
VALUES (nextval('approval_request_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)
ON CONFLICT (tenant_id, request_id, execution_step_id) DO NOTHING
RETURNING id`,
	qApprovalGet: `
SELECT ` + approvalColumns + `
  FROM approval_request
 WHERE tenant_id = ? AND request_id = ? AND execution_step_id = ?`,
	// The digest guard is in the WHERE clause, so approving a changed action
	// updates nothing rather than resolving the wrong proposal.
	qApprovalResolve: `
UPDATE approval_request
   SET status_code = ?, resolved_at = ?
 WHERE tenant_id = ? AND request_id = ? AND execution_step_id = ?
   AND status_code IN ('pending', 'escalated') AND proposed_digest = ?
RETURNING id`,
	qApprovalDecision: `
INSERT INTO approval_decision
       (approval_request_id, tenant_id, status_code, decider_kind, decider_user_id, decider_agent_id,
        decider_service_id, grant_id, proposed_digest, reason, decided_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qApprovalDecisionGet: `
SELECT d.status_code, d.decider_kind, d.decider_user_id, d.decider_agent_id, d.decider_service_id,
       d.grant_id, d.proposed_digest, d.reason
  FROM approval_decision d
  JOIN approval_request r ON r.id = d.approval_request_id
 WHERE r.tenant_id = ? AND r.request_id = ? AND r.execution_step_id = ?`,
	qApprovalPending: `
SELECT ` + approvalColumns + `
  FROM approval_request r
  JOIN approval_risk_tier risk ON risk.code = r.risk_tier_code
 WHERE r.tenant_id = ? AND r.status_code IN ('pending', 'escalated')
   AND (? = '' OR approver_kind = ?)
   AND (? = '' OR approver_id = ?)
   AND (? = '' OR risk.severity_rank >= (SELECT severity_rank FROM approval_risk_tier WHERE code = ?))
 ORDER BY CASE WHEN deadline_at IS NULL THEN 1 ELSE 0 END, deadline_at, id
 LIMIT ?`,
	qApprovalPendingScope: `
SELECT ` + approvalColumns + `
  FROM approval_request r
  JOIN approval_risk_tier risk ON risk.code = r.risk_tier_code
 WHERE r.tenant_id = ? AND r.status_code IN ('pending', 'escalated') AND r.scope_id = ?
   AND (? = '' OR approver_kind = ?)
   AND (? = '' OR approver_id = ?)
   AND (? = '' OR risk.severity_rank >= (SELECT severity_rank FROM approval_risk_tier WHERE code = ?))
 ORDER BY CASE WHEN deadline_at IS NULL THEN 1 ELSE 0 END, deadline_at, id
 LIMIT ?`,
	qApprovalDue: `
SELECT ` + approvalColumns + `
  FROM approval_request
 WHERE tenant_id = ? AND status_code IN ('pending', 'escalated')
   AND deadline_at IS NOT NULL AND deadline_at <= ?
 ORDER BY deadline_at
 LIMIT ?`,
	qApprovalEscalate: `
UPDATE approval_request
   SET status_code = 'escalated', approver_kind = ?, approver_id = ?, deadline_at = ?
 WHERE tenant_id = ? AND id = ? AND status_code IN ('pending', 'escalated')
RETURNING id`,
}

// TableStore is the durable ApprovalStore, inbox, and escalation writer.
type TableStore struct {
	DB  keelport.DatabaseRepository
	Now func() time.Time

	once sync.Once
	qs   keelport.QueryService
}

func (s *TableStore) init(ctx context.Context) error {
	if s.DB == nil {
		return fmt.Errorf("approval store: database is required")
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, approvalQueries) })
	if s.qs == nil {
		return fmt.Errorf("approval store: query service is required")
	}
	return nil
}

func (s *TableStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Open records what is owed. A replayed turn re-attaches to the request it
// already created rather than asking a human the same question twice.
func (s *TableStore) Open(ctx context.Context, request domain.ApprovalRequest) (domain.ApprovalRequest, error) {
	if err := s.init(ctx); err != nil {
		return domain.ApprovalRequest{}, err
	}
	if err := validateOpen(request); err != nil {
		return domain.ApprovalRequest{}, err
	}
	ctx = context.WithoutCancel(ctx)
	_, err := s.qs.Query(ctx, qApprovalOpen,
		request.TenantID, request.RequestID, request.ConversationID, request.ExecutionStepID,
		string(request.Principal.Kind), request.Principal.ID,
		nullable(string(request.Approver.Kind)), nullable(request.Approver.ID),
		nullable(request.ScopeID), nullable(request.RuleID), request.Action, request.Resource,
		string(request.Class), string(request.RiskTier), request.Summary,
		nullable(request.Evidence.URI), nullable(request.Evidence.Digest), request.ProposedDigest,
		nullableTime(request.DeadlineAt))
	if err != nil {
		return domain.ApprovalRequest{}, fmt.Errorf("open approval: %w", err)
	}
	stored, err := s.Get(ctx, domain.ApprovalKey{TenantID: request.TenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID})
	if err != nil {
		return domain.ApprovalRequest{}, err
	}
	if stored.ProposedDigest != request.ProposedDigest {
		return domain.ApprovalRequest{}, fmt.Errorf("%w: step %d already awaits a different proposal",
			domain.ErrConflict, request.ExecutionStepID)
	}
	return stored, nil
}

// Get returns one request by its key.
func (s *TableStore) Get(ctx context.Context, key domain.ApprovalKey) (domain.ApprovalRequest, error) {
	if err := s.init(ctx); err != nil {
		return domain.ApprovalRequest{}, err
	}
	result, err := s.qs.Query(ctx, qApprovalGet, key.TenantID, key.RequestID, key.ExecutionStepID)
	if err != nil {
		return domain.ApprovalRequest{}, fmt.Errorf("get approval: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.ApprovalRequest{}, fmt.Errorf("%w: approval for request %q step %d",
			domain.ErrNotFound, key.RequestID, key.ExecutionStepID)
	}
	return scanRequest(key.TenantID, result.Rows[0]), nil
}

// Resolve records the verdict once. A digest mismatch, a second verdict, or a
// terminal request all fail rather than silently overwrite.
func (s *TableStore) Resolve(ctx context.Context, verdict domain.ApprovalVerdict) (domain.ApprovalRequest, error) {
	if err := s.init(ctx); err != nil {
		return domain.ApprovalRequest{}, err
	}
	if err := validateVerdict(verdict); err != nil {
		return domain.ApprovalRequest{}, err
	}
	decidedAt := verdict.DecidedAt
	if decidedAt.IsZero() {
		decidedAt = s.now()
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := s.DB.BeginTx(ctx, approvalQueries)
	if err != nil {
		return domain.ApprovalRequest{}, fmt.Errorf("resolve approval: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	key := verdict.RequestKey
	updated, err := tx.Query(ctx, qApprovalResolve, string(verdict.Status), decidedAt,
		key.TenantID, key.RequestID, key.ExecutionStepID, verdict.ProposedDigest)
	if err != nil {
		return domain.ApprovalRequest{}, fmt.Errorf("resolve approval: %w", err)
	}
	if len(updated.Rows) == 0 {
		existing, queryErr := tx.Query(ctx, qApprovalDecisionGet, key.TenantID, key.RequestID, key.ExecutionStepID)
		if queryErr != nil {
			return domain.ApprovalRequest{}, fmt.Errorf("resolve approval: inspect existing verdict: %w", queryErr)
		}
		if len(existing.Rows) == 0 || !sameVerdict(existing.Rows[0], verdict) {
			return domain.ApprovalRequest{}, fmt.Errorf("%w: request %q step %d is not awaiting this proposal",
				domain.ErrConflict, key.RequestID, key.ExecutionStepID)
		}
		stored, queryErr := tx.Query(ctx, qApprovalGet, key.TenantID, key.RequestID, key.ExecutionStepID)
		if queryErr != nil {
			return domain.ApprovalRequest{}, fmt.Errorf("resolve approval: load existing verdict: %w", queryErr)
		}
		if len(stored.Rows) == 0 {
			return domain.ApprovalRequest{}, fmt.Errorf("%w: approval for request %q step %d",
				domain.ErrNotFound, key.RequestID, key.ExecutionStepID)
		}
		return scanRequest(key.TenantID, stored.Rows[0]), nil
	}
	id := common.AsInt64(updated.Rows[0][0])
	var deciderUser, deciderAgent, deciderService any
	switch verdict.Decider.Kind {
	case domain.PrincipalHuman:
		deciderUser, _ = strconv.ParseInt(verdict.Decider.ID, 10, 64)
	case domain.PrincipalAgent:
		deciderAgent = verdict.Decider.ID
	case domain.PrincipalService:
		deciderService = verdict.Decider.ID
	}
	if _, err = tx.Query(ctx, qApprovalDecision, id, key.TenantID, string(verdict.Status),
		string(verdict.Decider.Kind), deciderUser, deciderAgent, deciderService, nullable(verdict.Authority.GrantID),
		verdict.ProposedDigest, nullable(verdict.Reason), decidedAt); err != nil {
		return domain.ApprovalRequest{}, fmt.Errorf("record approval decision: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.ApprovalRequest{}, fmt.Errorf("resolve approval: commit: %w", err)
	}
	committed = true
	return s.Get(ctx, key)
}

// Pending returns the work one reviewer owes, soonest deadline first.
func (s *TableStore) Pending(ctx context.Context, filter domain.ApprovalFilter) ([]domain.ApprovalRequest, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	if filter.TenantID <= 0 {
		return nil, fmt.Errorf("%w: an approval inbox is always tenant-scoped", domain.ErrValidation)
	}
	if filter.MinRisk != "" && !validRisk(filter.MinRisk) {
		return nil, fmt.Errorf("%w: unknown minimum risk tier %q", domain.ErrValidation, filter.MinRisk)
	}
	kind, id := string(filter.Approver.Kind), filter.Approver.ID
	limit := pageLimit(filter.Limit)
	if len(filter.ScopeIDs) == 0 {
		result, err := s.qs.Query(ctx, qApprovalPending, filter.TenantID, kind, kind, id, id,
			string(filter.MinRisk), string(filter.MinRisk), limit)
		if err != nil {
			return nil, fmt.Errorf("list pending approvals: %w", err)
		}
		return scanRequests(filter.TenantID, result.Rows), nil
	}
	seenScopes := make(map[string]struct{}, len(filter.ScopeIDs))
	requests := make([]domain.ApprovalRequest, 0)
	for _, scopeID := range filter.ScopeIDs {
		if strings.TrimSpace(scopeID) == "" {
			return nil, fmt.Errorf("%w: approval scope ids cannot be blank", domain.ErrValidation)
		}
		if _, duplicate := seenScopes[scopeID]; duplicate {
			continue
		}
		seenScopes[scopeID] = struct{}{}
		result, err := s.qs.Query(ctx, qApprovalPendingScope, filter.TenantID, scopeID, kind, kind, id, id,
			string(filter.MinRisk), string(filter.MinRisk), limit)
		if err != nil {
			return nil, fmt.Errorf("list pending approvals for scope %q: %w", scopeID, err)
		}
		requests = append(requests, scanRequests(filter.TenantID, result.Rows)...)
	}
	sort.Slice(requests, func(i, j int) bool {
		left, right := requests[i].DeadlineAt, requests[j].DeadlineAt
		if left.IsZero() != right.IsZero() {
			return !left.IsZero()
		}
		if !left.Equal(right) {
			return left.Before(right)
		}
		return requests[i].ID < requests[j].ID
	})
	if len(requests) > limit {
		requests = requests[:limit]
	}
	return requests, nil
}

// DueBy returns requests whose deadline has passed with no verdict.
func (s *TableStore) DueBy(ctx context.Context, tenantID int64, filter domain.ApprovalFilter) ([]domain.ApprovalRequest, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	deadline := filter.DueBefore
	if deadline.IsZero() {
		deadline = s.now()
	}
	result, err := s.qs.Query(ctx, qApprovalDue, tenantID, deadline, pageLimit(filter.Limit))
	if err != nil {
		return nil, fmt.Errorf("list due approvals: %w", err)
	}
	return scanRequests(tenantID, result.Rows), nil
}

// Reassign moves a request to a backup approver with a new deadline.
func (s *TableStore) Reassign(ctx context.Context, request domain.ApprovalRequest, step domain.EscalationStep) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(step.Backup.ID) == "" {
		return fmt.Errorf("%w: escalation needs a backup approver", domain.ErrValidation)
	}
	deadline := s.now().Add(step.Extension)
	ctx = context.WithoutCancel(ctx)
	updated, err := s.qs.Query(ctx, qApprovalEscalate, string(step.Backup.Kind), step.Backup.ID, deadline, request.TenantID, request.ID)
	if err != nil {
		return fmt.Errorf("escalate approval: %w", err)
	}
	if len(updated.Rows) == 0 {
		return fmt.Errorf("%w: approval %d is no longer open", domain.ErrConflict, request.ID)
	}
	return nil
}

func validateOpen(request domain.ApprovalRequest) error {
	switch {
	case request.TenantID <= 0 || strings.TrimSpace(request.RequestID) == "" || request.ExecutionStepID <= 0:
		return fmt.Errorf("%w: approval needs tenant, request, and execution step", domain.ErrValidation)
	case request.Principal.Kind == "" || strings.TrimSpace(request.Principal.ID) == "":
		return fmt.Errorf("%w: approval needs the acting principal", domain.ErrPrincipalUnknown)
	case len(request.ProposedDigest) != 64:
		return fmt.Errorf("%w: approval needs the digest of the proposed action", domain.ErrValidation)
	case request.Class == "" || request.RiskTier == "":
		return fmt.Errorf("%w: approval needs an output class and a risk tier", domain.ErrValidation)
	}
	return nil
}

func validateVerdict(verdict domain.ApprovalVerdict) error {
	switch verdict.Status {
	case domain.ApprovalStatusApproved, domain.ApprovalStatusRejected, domain.ApprovalStatusEdited,
		domain.ApprovalStatusExpired, domain.ApprovalStatusWithdrawn:
	default:
		return fmt.Errorf("%w: %q is not a terminal verdict", domain.ErrValidation, verdict.Status)
	}
	if verdict.Decider.Kind == "" || strings.TrimSpace(verdict.Decider.ID) == "" {
		return fmt.Errorf("%w: a verdict needs the principal that decided", domain.ErrPrincipalUnknown)
	}
	switch verdict.Decider.Kind {
	case domain.PrincipalHuman:
		if _, err := strconv.ParseInt(verdict.Decider.ID, 10, 64); err != nil {
			return fmt.Errorf("%w: human decider id must be a user account id", domain.ErrValidation)
		}
	case domain.PrincipalAgent, domain.PrincipalService:
	default:
		return fmt.Errorf("%w: unknown decider kind %q", domain.ErrValidation, verdict.Decider.Kind)
	}
	if len(verdict.ProposedDigest) != 64 {
		return fmt.Errorf("%w: a verdict must name the proposal it decides", domain.ErrValidation)
	}
	return nil
}

func sameVerdict(row []any, verdict domain.ApprovalVerdict) bool {
	deciderID := common.AsString(row[3])
	switch verdict.Decider.Kind {
	case domain.PrincipalHuman:
		deciderID = strconv.FormatInt(common.AsInt64(row[2]), 10)
	case domain.PrincipalService:
		deciderID = common.AsString(row[4])
	}
	return common.AsString(row[0]) == string(verdict.Status) &&
		common.AsString(row[1]) == string(verdict.Decider.Kind) && deciderID == verdict.Decider.ID &&
		common.AsString(row[5]) == verdict.Authority.GrantID && common.AsString(row[6]) == verdict.ProposedDigest &&
		common.AsString(row[7]) == verdict.Reason
}

func scanRequests(tenantID int64, rows [][]any) []domain.ApprovalRequest {
	requests := make([]domain.ApprovalRequest, 0, len(rows))
	for _, row := range rows {
		requests = append(requests, scanRequest(tenantID, row))
	}
	return requests
}

func scanRequest(tenantID int64, row []any) domain.ApprovalRequest {
	deadline, _ := common.AsTimeOK(row[19])
	resolved, _ := common.AsTimeOK(row[21])
	return domain.ApprovalRequest{
		ID: common.AsInt64(row[0]), TenantID: tenantID, RequestID: common.AsString(row[1]),
		ConversationID: common.AsString(row[2]), ExecutionStepID: common.AsInt64(row[3]),
		Principal: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[4])), ID: common.AsString(row[5])},
		Approver:  domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[6])), ID: common.AsString(row[7])},
		ScopeID:   common.AsString(row[8]), RuleID: common.AsString(row[9]),
		Action: common.AsString(row[10]), Resource: common.AsString(row[11]),
		Class: domain.OutputClass(common.AsString(row[12])), RiskTier: domain.RiskTier(common.AsString(row[13])),
		Summary:        common.AsString(row[14]),
		Evidence:       domain.ObjectRef{URI: common.AsString(row[15]), Digest: common.AsString(row[16])},
		ProposedDigest: common.AsString(row[17]), Status: domain.ApprovalStatus(common.AsString(row[18])),
		DeadlineAt: deadline, CreatedAt: common.AsTime(row[20]), ResolvedAt: resolved,
	}
}

func pageLimit(limit int) int {
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}

func validRisk(risk domain.RiskTier) bool {
	switch risk {
	case domain.RiskLow, domain.RiskMedium, domain.RiskHigh, domain.RiskCritical:
		return true
	default:
		return false
	}
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

var (
	_ contract.ApprovalStore = (*TableStore)(nil)
	_ contract.ApprovalInbox = (*TableStore)(nil)
)
