// Package approval is the durable human-in-the-loop path: a governed action that
// needs a person parks the turn, records what is owed, and resumes on the verdict.
// Nothing here decides who may approve — that is the policy decision point.
package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/outbox"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
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
	qApprovalNotified     = "scout_approval_notified"
	qApprovalRevision     = "scout_approval_revision"
	qApprovalVacant       = "scout_approval_vacant"
	qApprovalUnsettled    = "scout_approval_unsettled"
	qApprovalTurnSettled  = "scout_approval_turn_settled"

	// maxResolutionBytes bounds the resolver's evidence reference kept on a routed request.
	maxResolutionBytes = 256

	// ApprovalAggregate and ApprovalRequestedEvent route the outbox event that notifies an approver.
	ApprovalAggregate      = "turn_approval_request"
	ApprovalRequestedEvent = "approval.requested"

	approvalColumns = `id, request_id, conversation_id, execution_step_id, principal_kind, principal_id,
       approver_kind, approver_id, scope_id, rule_id, requested_action, resource_ref, output_class_code,
       risk_tier_code, summary, evidence_uri, evidence_digest, proposed_digest, status_code,
       deadline_at, created_at, resolved_at, notification_event_id, revises_step_id, release_version,
       requester_kind, requester_id, rule_source_code, approval_route_id, approver_reference_kind,
       approver_reference, route_step, route_resolution, authenticated_only`

	sqlApprovalPending = "'" + string(domain.ApprovalStatusPending) + "'"
	sqlApprovalEdited  = "'" + string(domain.ApprovalStatusEdited) + "'"
	sqlApprovalExpired = "'" + string(domain.ApprovalStatusExpired) + "'"
	// sqlApprovalOpen lists the statuses that still owe a verdict.
	sqlApprovalOpen = "(" + sqlApprovalPending + ", '" + string(domain.ApprovalStatusEscalated) + "')"
)

var approvalQueries = map[string]string{
	qApprovalOpen: `
INSERT INTO turn_approval_request
       (id, partner_id, request_id, conversation_id, execution_step_id, principal_kind, principal_id,
        approver_kind, approver_id, scope_id, rule_id, requested_action, resource_ref, output_class_code,
        risk_tier_code, summary, evidence_uri, evidence_digest, proposed_digest, status_code, deadline_at, revises_step_id,
        release_version, requester_kind, requester_id, rule_source_code, approval_route_id, approver_reference_kind,
        approver_reference, route_step, route_resolution, authenticated_only)
VALUES (nextval('turn_approval_request_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ` + sqlApprovalPending + `, ?, ?,
        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (partner_id, request_id, execution_step_id) DO NOTHING
RETURNING id`,
	qApprovalGet: `
SELECT ` + approvalColumns + `
  FROM turn_approval_request
 WHERE partner_id = ? AND request_id = ? AND execution_step_id = ?`,
	// The digest and assignment guards are in the WHERE clause, so approving a changed action,
	// or deciding a request reassigned since it was read, updates nothing.
	qApprovalResolve: `
UPDATE turn_approval_request
   SET status_code = ?, resolved_at = ?
 WHERE partner_id = ? AND request_id = ? AND execution_step_id = ?
   AND status_code IN ` + sqlApprovalOpen + ` AND proposed_digest = ?
   AND route_step = ? AND approver_kind IS NOT DISTINCT FROM ? AND approver_id IS NOT DISTINCT FROM ?
RETURNING id`,
	qApprovalDecision: `
INSERT INTO turn_approval_decision
       (turn_approval_request_id, partner_id, status_code, decider_kind, decider_user_id, decider_agent_id,
        decider_service_id, grant_id, proposed_digest, reason, decided_at, revised_arguments_uri, revised_arguments_digest)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	qApprovalDecisionGet: `
SELECT d.status_code, d.decider_kind, d.decider_user_id, d.decider_agent_id, d.decider_service_id,
       d.grant_id, d.proposed_digest, d.reason, d.revised_arguments_digest
  FROM turn_approval_decision d
  JOIN turn_approval_request r ON r.id = d.turn_approval_request_id
 WHERE r.partner_id = ? AND r.request_id = ? AND r.execution_step_id = ?`,
	qApprovalRevision: `
SELECT d.decider_kind, d.decider_user_id, d.decider_agent_id, d.decider_service_id, d.grant_id, d.reason,
       d.revised_arguments_uri, d.revised_arguments_digest
  FROM turn_approval_decision d
  JOIN turn_approval_request r ON r.id = d.turn_approval_request_id
 WHERE r.partner_id = ? AND r.request_id = ? AND r.execution_step_id = ?
   AND d.status_code = ` + sqlApprovalEdited + ` AND d.revised_arguments_uri IS NOT NULL`,
	qApprovalPending: `
SELECT ` + approvalColumns + `
  FROM turn_approval_request r
  JOIN approval_risk_tier risk ON risk.code = r.risk_tier_code
 WHERE r.partner_id = ? AND r.status_code IN ` + sqlApprovalOpen + `
   AND (? = '' OR approver_kind = ?)
   AND (? = '' OR approver_id = ?)
   AND (? = '' OR risk.severity_rank >= (SELECT severity_rank FROM approval_risk_tier WHERE code = ?))
 ORDER BY CASE WHEN deadline_at IS NULL THEN 1 ELSE 0 END, deadline_at, id
 LIMIT ?`,
	qApprovalPendingScope: `
SELECT ` + approvalColumns + `
  FROM turn_approval_request r
  JOIN approval_risk_tier risk ON risk.code = r.risk_tier_code
 WHERE r.partner_id = ? AND r.status_code IN ` + sqlApprovalOpen + ` AND r.scope_id = ?
   AND (? = '' OR approver_kind = ?)
   AND (? = '' OR approver_id = ?)
   AND (? = '' OR risk.severity_rank >= (SELECT severity_rank FROM approval_risk_tier WHERE code = ?))
 ORDER BY CASE WHEN deadline_at IS NULL THEN 1 ELSE 0 END, deadline_at, id
 LIMIT ?`,
	qApprovalDue: `
SELECT ` + approvalColumns + `
  FROM turn_approval_request
 WHERE partner_id = ? AND status_code IN ` + sqlApprovalOpen + `
   AND deadline_at IS NOT NULL AND deadline_at <= ?
 ORDER BY deadline_at
 LIMIT ?`,
	qApprovalNotified: `
UPDATE turn_approval_request
   SET notification_event_id = ?
 WHERE partner_id = ? AND id = ? AND notification_event_id IS NULL`,
	// Reassignment is a compare-and-swap on the assignment it was decided against.
	qApprovalEscalate: `
UPDATE turn_approval_request
   SET status_code = ?, approver_kind = ?, approver_id = ?, deadline_at = ?, route_step = ?,
       approver_reference_kind = ?, approver_reference = ?, route_resolution = ?
 WHERE partner_id = ? AND id = ? AND status_code IN ` + sqlApprovalOpen + `
   AND route_step = ? AND approver_kind IS NOT DISTINCT FROM ? AND approver_id IS NOT DISTINCT FROM ?
RETURNING id`,
	qApprovalVacant: `
SELECT ` + approvalColumns + `
  FROM turn_approval_request
 WHERE partner_id = ? AND status_code IN ` + sqlApprovalOpen + ` AND approval_route_id IS NOT NULL
   AND approver_id IS NULL AND (deadline_at IS NULL OR deadline_at > ?)
 ORDER BY deadline_at NULLS LAST, id
 LIMIT ?`,
	qApprovalUnsettled: `
SELECT ` + approvalColumns + `
  FROM turn_approval_request
 WHERE partner_id = ? AND status_code = ` + sqlApprovalExpired + ` AND turn_settled_at IS NULL
 ORDER BY resolved_at, id
 LIMIT ?`,
	qApprovalTurnSettled: `
UPDATE turn_approval_request
   SET turn_settled_at = ?
 WHERE partner_id = ? AND id = ? AND status_code = ` + sqlApprovalExpired + ` AND turn_settled_at IS NULL`,
}

// TableStore is the durable ApprovalStore, inbox, and escalation writer.
type TableStore struct {
	DB  keelport.DatabaseRepository
	Now func() time.Time
	// NotifyThroughOutbox queues the first approver's notification as a keel outbox
	// event in the transaction that opens the request, so a request never exists
	// without its notice; a keel outbox worker's Dispatcher delivers it. Leave the
	// gate's Notifier unset when this is on.
	NotifyThroughOutbox bool

	once sync.Once
	qs   keelport.QueryService
}

// approvalNotice is the outbox payload: who owes which decision, never its evidence.
type approvalNotice struct {
	TenantID        int64     `json:"tenant_id"`
	RecipientKind   string    `json:"recipient_kind"`
	RecipientID     string    `json:"recipient_id"`
	RequestID       string    `json:"request_id"`
	ExecutionStepID int64     `json:"execution_step_id"`
	Subject         string    `json:"subject"`
	RiskTier        string    `json:"risk_tier"`
	DueAt           time.Time `json:"due_at,omitzero"`
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
	return clk.Now(s.Now)
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
	var err error
	if s.NotifyThroughOutbox {
		err = s.openNotified(ctx, request)
	} else {
		_, err = insertRequest(ctx, s.qs, request)
	}
	if err != nil {
		return domain.ApprovalRequest{}, err
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

// openNotified inserts the request and, only when this call created it, queues the
// approver's notice in the same transaction; a replayed turn notifies no one again.
func (s *TableStore) openNotified(ctx context.Context, request domain.ApprovalRequest) error {
	queries := maps.Clone(approvalQueries)
	maps.Copy(queries, outbox.WriteQueries())
	tx, err := s.DB.BeginTx(ctx, queries)
	if err != nil {
		return fmt.Errorf("open approval: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	id, err := insertRequest(ctx, tx, request)
	if err != nil {
		return err
	}
	if id > 0 && request.Approver.ID != "" {
		payload, err := json.Marshal(approvalNotice{
			TenantID: request.TenantID, RecipientKind: string(request.Approver.Kind), RecipientID: request.Approver.ID,
			RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID, Subject: request.Action,
			RiskTier: string(request.RiskTier), DueAt: request.DeadlineAt,
		})
		if err != nil {
			return fmt.Errorf("encode approval notice: %w", err)
		}
		eventID, err := outbox.EnqueueTx(ctx, tx, outbox.Event{PartnerID: request.TenantID, AggregateType: ApprovalAggregate,
			AggregateID: strconv.FormatInt(id, 10), EventType: ApprovalRequestedEvent, Payload: string(payload)})
		if err != nil {
			return fmt.Errorf("queue approval notice: %w", err)
		}
		if _, err = tx.Query(ctx, qApprovalNotified, eventID, request.TenantID, id); err != nil {
			return fmt.Errorf("link approval notice: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("open approval: commit: %w", err)
	}
	committed = true
	return nil
}

// insertRequest returns the new request's id, or zero when the key already exists.
func insertRequest(ctx context.Context, qs keelport.QueryService, request domain.ApprovalRequest) (int64, error) {
	inserted, err := qs.Query(ctx, qApprovalOpen,
		request.TenantID, request.RequestID, request.ConversationID, request.ExecutionStepID,
		string(request.Principal.Kind), request.Principal.ID,
		common.NullIfEmpty(string(request.Approver.Kind)), common.NullIfEmpty(request.Approver.ID),
		common.NullIfEmpty(request.ScopeID), request.RuleID, request.Action, request.Resource,
		string(request.Class), string(request.RiskTier), request.Summary,
		common.NullIfEmpty(request.Evidence.URI), common.NullIfEmpty(request.Evidence.Digest), request.ProposedDigest,
		nullableTime(request.DeadlineAt), nullableID(request.RevisesStepID),
		common.NullIfEmpty(request.ReleaseVersion), common.NullIfEmpty(string(request.Requester.Kind)), common.NullIfEmpty(request.Requester.ID),
		string(request.Source), common.NullIfEmpty(request.Route.RouteID), common.NullIfEmpty(string(request.Route.Reference.Kind)),
		common.NullIfEmpty(request.Route.Reference.Ref), request.Route.Step, common.NullIfEmpty(request.Route.Resolution), request.Route.AuthenticatedOnly)
	if err != nil {
		return 0, fmt.Errorf("open approval: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return 0, nil
	}
	return common.AsInt64(inserted.Rows[0][0]), nil
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
	fence := verdict.Assignment
	updated, err := tx.Query(ctx, qApprovalResolve, string(verdict.Status), decidedAt,
		key.TenantID, key.RequestID, key.ExecutionStepID, verdict.ProposedDigest,
		fence.Step, common.NullIfEmpty(string(fence.Approver.Kind)), common.NullIfEmpty(fence.Approver.ID))
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
		string(verdict.Decider.Kind), deciderUser, deciderAgent, deciderService, common.NullIfEmpty(verdict.Authority.GrantID),
		verdict.ProposedDigest, common.NullIfEmpty(verdict.Reason), decidedAt,
		common.NullIfEmpty(verdict.Revision.URI), common.NullIfEmpty(verdict.Revision.Digest)); err != nil {
		return domain.ApprovalRequest{}, fmt.Errorf("record approval decision: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.ApprovalRequest{}, fmt.Errorf("resolve approval: commit: %w", err)
	}
	committed = true
	return s.Get(ctx, key)
}

// Revision returns the edit recorded on an edited request.
func (s *TableStore) Revision(ctx context.Context, key domain.ApprovalKey) (domain.ProposalRevision, error) {
	if err := s.init(ctx); err != nil {
		return domain.ProposalRevision{}, err
	}
	result, err := s.qs.Query(ctx, qApprovalRevision, key.TenantID, key.RequestID, key.ExecutionStepID)
	if err != nil {
		return domain.ProposalRevision{}, fmt.Errorf("read approval revision: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.ProposalRevision{}, fmt.Errorf("%w: no edit of request %q step %d",
			domain.ErrNotFound, key.RequestID, key.ExecutionStepID)
	}
	row := result.Rows[0]
	editor := domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[0]))}
	switch editor.Kind {
	case domain.PrincipalHuman:
		editor.ID = strconv.FormatInt(common.AsInt64(row[1]), 10)
	case domain.PrincipalAgent:
		editor.ID = common.AsString(row[2])
	default:
		editor.ID = common.AsString(row[3])
	}
	return domain.ProposalRevision{
		Original: key, Editor: editor,
		Authority: domain.AuthorityRef{Subject: editor, GrantID: common.AsString(row[4])},
		Reason:    common.AsString(row[5]),
		Arguments: domain.ObjectRef{URI: common.AsString(row[6]), Digest: common.AsString(row[7])},
	}, nil
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

// Vacant returns open routed requests nobody is assigned yet whose deadline has not passed,
// so a sweep can fill them once someone becomes eligible.
func (s *TableStore) Vacant(ctx context.Context, tenantID int64, limit int) ([]domain.ApprovalRequest, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	result, err := s.qs.Query(ctx, qApprovalVacant, tenantID, s.now(), pageLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("list vacant approvals: %w", err)
	}
	return scanRequests(tenantID, result.Rows), nil
}

// Unsettled returns expired requests whose parked turn is not yet known to be failed.
func (s *TableStore) Unsettled(ctx context.Context, tenantID int64, limit int) ([]domain.ApprovalRequest, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	result, err := s.qs.Query(ctx, qApprovalUnsettled, tenantID, pageLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("list unsettled approvals: %w", err)
	}
	return scanRequests(tenantID, result.Rows), nil
}

// TurnSettled records that the turn parked on an expired request is failed or no longer parked.
func (s *TableStore) TurnSettled(ctx context.Context, tenantID, id int64) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if _, err := s.qs.Query(ctx, qApprovalTurnSettled, s.now(), tenantID, id); err != nil {
		return fmt.Errorf("settle turn of approval %d: %w", id, err)
	}
	return nil
}

// Reassign applies one escalation step to request as it was read. It swaps the assignment only
// while the request still holds the step and approver it was read with; otherwise another sweep
// or a verdict won and the result is domain.ErrConflict.
func (s *TableStore) Reassign(ctx context.Context, request domain.ApprovalRequest, step domain.EscalationStep) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	if err := validateReassign(request, step); err != nil {
		return err
	}
	var deadline any
	switch {
	case step.KeepDeadline:
		deadline = nullableTime(request.DeadlineAt)
	case step.Extension > 0:
		deadline = s.now().Add(step.Extension)
	}
	status := domain.ApprovalStatusEscalated
	if step.KeepDeadline && request.Status == domain.ApprovalStatusPending {
		status = domain.ApprovalStatusPending
	}
	backup, route := step.Backup, step.Route
	if request.Route.RouteID == "" {
		route = domain.ApprovalRouteAuthority{}
	}
	args := []any{string(status), common.NullIfEmpty(string(backup.Kind)), common.NullIfEmpty(backup.ID), deadline, route.Step,
		common.NullIfEmpty(string(route.Reference.Kind)), common.NullIfEmpty(route.Reference.Ref), common.NullIfEmpty(route.Resolution),
		request.TenantID, request.ID, request.Route.Step, common.NullIfEmpty(string(request.Approver.Kind)), common.NullIfEmpty(request.Approver.ID)}
	ctx = context.WithoutCancel(ctx)
	if !s.NotifyThroughOutbox || backup.ID == "" || backup == request.Approver {
		updated, err := s.qs.Query(ctx, qApprovalEscalate, args...)
		if err != nil {
			return fmt.Errorf("escalate approval: %w", err)
		}
		if len(updated.Rows) == 0 {
			return fmt.Errorf("%w: approval %d is no longer assigned as read", domain.ErrConflict, request.ID)
		}
		return nil
	}
	return s.reassignNotified(ctx, request, backup, deadline, args)
}

// reassignNotified swaps the assignment and queues the new approver's notice in one transaction.
func (s *TableStore) reassignNotified(ctx context.Context, request domain.ApprovalRequest, backup domain.PrincipalRef, deadline any, args []any) error {
	queries := maps.Clone(approvalQueries)
	maps.Copy(queries, outbox.WriteQueries())
	tx, err := s.DB.BeginTx(ctx, queries)
	if err != nil {
		return fmt.Errorf("escalate approval: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	updated, err := tx.Query(ctx, qApprovalEscalate, args...)
	if err != nil {
		return fmt.Errorf("escalate approval: %w", err)
	}
	if len(updated.Rows) == 0 {
		return fmt.Errorf("%w: approval %d is no longer assigned as read", domain.ErrConflict, request.ID)
	}
	due, _ := deadline.(time.Time)
	payload, err := json.Marshal(approvalNotice{
		TenantID: request.TenantID, RecipientKind: string(backup.Kind), RecipientID: backup.ID,
		RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID, Subject: request.Action,
		RiskTier: string(request.RiskTier), DueAt: due,
	})
	if err != nil {
		return fmt.Errorf("encode approval notice: %w", err)
	}
	if _, err = outbox.EnqueueTx(ctx, tx, outbox.Event{PartnerID: request.TenantID, AggregateType: ApprovalAggregate,
		AggregateID: strconv.FormatInt(request.ID, 10), EventType: ApprovalRequestedEvent, Payload: string(payload)}); err != nil {
		return fmt.Errorf("queue approval notice: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("escalate approval: commit: %w", err)
	}
	committed = true
	return nil
}

func validateReassign(request domain.ApprovalRequest, step domain.EscalationStep) error {
	routed := request.Route.RouteID != ""
	backup := step.Backup
	if (backup.Kind == "") != (strings.TrimSpace(backup.ID) == "") {
		return fmt.Errorf("%w: an approver needs both kind and id", domain.ErrValidation)
	}
	switch step.Resolved() {
	case domain.EscalationReassign:
		if backup.ID == "" {
			return fmt.Errorf("%w: a reassignment needs a backup approver", domain.ErrValidation)
		}
		if routed && !decimalHuman(backup) {
			return fmt.Errorf("%w: a routed approver is a person with a user account id", domain.ErrValidation)
		}
		if step.KeepDeadline && request.Approver.ID != "" {
			return fmt.Errorf("%w: only a vacancy is filled under its current deadline", domain.ErrValidation)
		}
	case domain.EscalationVacate:
		if !routed || backup.ID != "" {
			return fmt.Errorf("%w: only a routed request is vacated, and to nobody", domain.ErrValidation)
		}
	case domain.EscalationHold:
		if backup != request.Approver || (routed && step.Route.Step != request.Route.Step) || step.KeepDeadline || step.Extension != 0 {
			return fmt.Errorf("%w: a hold keeps the current assignment with no deadline", domain.ErrValidation)
		}
	default:
		return fmt.Errorf("%w: escalation action %q does not reassign", domain.ErrValidation, step.Resolved())
	}
	if step.KeepDeadline && step.Resolved() != domain.EscalationReassign {
		return fmt.Errorf("%w: only a reassignment keeps the current deadline", domain.ErrValidation)
	}
	if step.Extension < 0 {
		return fmt.Errorf("%w: an escalation extension cannot be negative", domain.ErrValidation)
	}
	if !routed {
		if step.Route.RouteID != "" || step.Route.Step != 0 {
			return fmt.Errorf("%w: an unrouted request has no route step", domain.ErrValidation)
		}
		return nil
	}
	if step.Route.RouteID != request.Route.RouteID {
		return fmt.Errorf("%w: escalation keeps the route a request was opened under", domain.ErrValidation)
	}
	if step.Route.Step < request.Route.Step || step.Route.Step > domain.MaxApprovalEscalationSteps {
		return fmt.Errorf("%w: escalation moves a route step forward, up to %d", domain.ErrValidation, domain.MaxApprovalEscalationSteps)
	}
	return validateAuthority(step.Route)
}

// validateAuthority checks the routed authority a request is assigned under.
func validateAuthority(route domain.ApprovalRouteAuthority) error {
	reference := route.Reference
	switch {
	case len(route.RouteID) > 64:
		return fmt.Errorf("%w: an approval route id holds at most 64 characters", domain.ErrValidation)
	case reference.Kind != domain.ApproverRole && reference.Kind != domain.ApproverPosition:
		return fmt.Errorf("%w: unknown approver reference kind %q", domain.ErrValidation, reference.Kind)
	case strings.TrimSpace(reference.Ref) == "" || len(reference.Ref) > 128:
		return fmt.Errorf("%w: an approver reference holds 1 to 128 characters", domain.ErrValidation)
	case route.Step < 0 || route.Step > domain.MaxApprovalEscalationSteps:
		return fmt.Errorf("%w: a route step is from 0 to %d", domain.ErrValidation, domain.MaxApprovalEscalationSteps)
	case len(route.Resolution) > maxResolutionBytes:
		return fmt.Errorf("%w: a route resolution holds at most %d bytes", domain.ErrValidation, maxResolutionBytes)
	}
	return nil
}

func decimalHuman(principal domain.PrincipalRef) bool {
	if principal.Kind != domain.PrincipalHuman {
		return false
	}
	id, err := strconv.ParseInt(principal.ID, 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == principal.ID
}

func validateOpen(request domain.ApprovalRequest) error {
	switch {
	case request.TenantID <= 0 || strings.TrimSpace(request.RequestID) == "" || request.ExecutionStepID <= 0:
		return fmt.Errorf("%w: approval needs tenant, request, and execution step", domain.ErrValidation)
	case request.Principal.Kind == "" || strings.TrimSpace(request.Principal.ID) == "":
		return fmt.Errorf("%w: approval needs the acting principal", domain.ErrPrincipalUnknown)
	case len(request.ProposedDigest) != 64:
		return fmt.Errorf("%w: approval needs the digest of the proposed action", domain.ErrValidation)
	case !request.Class.Valid() || request.RiskTier == "":
		return fmt.Errorf("%w: approval needs a known output class and a risk tier", domain.ErrValidation)
	case request.RevisesStepID < 0 || request.RevisesStepID == request.ExecutionStepID:
		return fmt.Errorf("%w: a revision revises another step of its turn", domain.ErrValidation)
	case (request.Approver.Kind == "") != (strings.TrimSpace(request.Approver.ID) == ""):
		return fmt.Errorf("%w: an approver needs both kind and id", domain.ErrValidation)
	case (request.Requester.Kind == "") != (strings.TrimSpace(request.Requester.ID) == ""):
		return fmt.Errorf("%w: a requester needs both kind and id", domain.ErrValidation)
	case !knownSource(request.Source):
		return fmt.Errorf("%w: unknown approval source %q", domain.ErrValidation, request.Source)
	case strings.TrimSpace(request.RuleID) == "" || len(request.RuleID) > maxRuleIDLength:
		return fmt.Errorf("%w: an approval names a rule of 1 to %d characters", domain.ErrValidation, maxRuleIDLength)
	case request.ReleaseVersion != "" && (request.Principal.Kind != domain.PrincipalAgent || len(request.ReleaseVersion) > 80):
		return fmt.Errorf("%w: only an agent's request names its release", domain.ErrValidation)
	}
	if request.Route.RouteID == "" {
		if request.Route != (domain.ApprovalRouteAuthority{}) {
			return fmt.Errorf("%w: an unrouted request carries no routed authority", domain.ErrValidation)
		}
		return nil
	}
	if request.ReleaseVersion == "" {
		return fmt.Errorf("%w: a routed request names the release whose routing applies", domain.ErrValidation)
	}
	if request.Approver.ID != "" && !decimalHuman(request.Approver) {
		return fmt.Errorf("%w: a routed approver is a person with a user account id", domain.ErrValidation)
	}
	return validateAuthority(request.Route)
}

func knownSource(source domain.ApprovalSource) bool {
	switch source {
	case domain.ApprovalSourceAutonomy, domain.ApprovalSourcePolicy, domain.ApprovalSourceGuardrail:
		return true
	default:
		return false
	}
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
	if verdict.Assignment.Step < 0 || verdict.Assignment.Step > domain.MaxApprovalEscalationSteps {
		return fmt.Errorf("%w: a verdict's assignment step is from 0 to %d", domain.ErrValidation, domain.MaxApprovalEscalationSteps)
	}
	revised := strings.TrimSpace(verdict.Revision.URI) != ""
	switch {
	case verdict.Status == domain.ApprovalStatusEdited && (!revised || len(verdict.Revision.Digest) != 64):
		return fmt.Errorf("%w: an edited verdict needs its archived revised arguments", domain.ErrValidation)
	case verdict.Status != domain.ApprovalStatusEdited && (revised || verdict.Revision.Digest != ""):
		return fmt.Errorf("%w: only an edited verdict carries revised arguments", domain.ErrValidation)
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
		common.AsString(row[7]) == verdict.Reason && common.AsString(row[8]) == verdict.Revision.Digest
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
	notified, _ := common.AsInt64OK(row[22])
	revises, _ := common.AsInt64OK(row[23])
	step, _ := common.AsInt64OK(row[31])
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
		NotificationEventID: notified, RevisesStepID: revises, ReleaseVersion: common.AsString(row[24]),
		Requester: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[25])), ID: common.AsString(row[26])},
		Source:    domain.ApprovalSource(common.AsString(row[27])),
		Route: domain.ApprovalRouteAuthority{
			RouteID: common.AsString(row[28]), Step: int(step), Resolution: common.AsString(row[32]), AuthenticatedOnly: common.AsBool(row[33]),
			Reference: domain.ApproverReference{Kind: domain.ApproverReferenceKind(common.AsString(row[29])), Ref: common.AsString(row[30])},
		},
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

func nullableID(value int64) any {
	if value == 0 {
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
