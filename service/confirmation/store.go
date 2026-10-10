package confirmation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nauticana/keel/approval"
	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/outbox"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qConfirmInsert         = "scout_mcp_confirmation_insert"
	qConfirmOpen           = "scout_mcp_confirmation_open"
	qConfirmGet            = "scout_mcp_confirmation_get"
	qConfirmNotified       = "scout_mcp_confirmation_notified"
	qConfirmAttach         = "scout_mcp_confirmation_attach"
	qConfirmAbandon        = "scout_mcp_confirmation_abandon"
	qConfirmDecide         = "scout_mcp_confirmation_decide"
	qConfirmDecideApproval = "scout_mcp_confirmation_decide_approval"
	qConfirmWithdraw       = "scout_mcp_confirmation_withdraw"
	qConfirmApproved       = "scout_mcp_confirmation_approved"
	qConfirmClaim          = "scout_mcp_confirmation_claim"
	qConfirmRenew          = "scout_mcp_confirmation_renew"
	qConfirmComplete       = "scout_mcp_confirmation_complete"
	qConfirmLapse          = "scout_mcp_confirmation_lapse"
	qConfirmExpire         = "scout_mcp_confirmation_expire"
	qConfirmReconcile      = "scout_mcp_confirmation_reconcile"
	qConfirmPurge          = "scout_mcp_confirmation_purge"
	qConfirmList           = "scout_mcp_confirmation_list"

	// Aggregate and PendingEvent name the outbox notice of an inbox confirmation.
	Aggregate    = "mcp_confirmation"
	PendingEvent = "mcp_confirmation.pending"
	// ApprovalObjectType is the keel approval_request object type of a confirmation.
	ApprovalObjectType = "mcp_confirmation"

	// MaxDocumentBytes bounds the stored payload, preview, requirements, and result.
	MaxDocumentBytes = 1 << 20
	// MaxNoteRunes bounds decision, failure, and reconciliation notes.
	MaxNoteRunes = 500
	// DefaultBatch and MaxBatch bound one list or sweep.
	DefaultBatch = 100
	MaxBatch     = 1000
)

const confirmationColumns = `id, partner_id, maker_kind, maker_id, COALESCE(client_ref, ''), tool, COALESCE(payload, ''),
       payload_digest, COALESCE(preview, ''), requirements, channel_code, status_code, approval_required, COALESCE(approval_id, 0),
       expires_at, created_at, decided_at, COALESCE(decider_kind, ''), COALESCE(decider_id, ''),
       COALESCE(decision_note, ''), fence, attempts, COALESCE(result, ''), COALESCE(error_text, ''), completed_at,
       COALESCE(reconciler_kind, ''), COALESCE(reconciler_id, ''), COALESCE(reconcile_note, ''),
       (status_code = 'pending' AND expires_at <= CURRENT_TIMESTAMP)`

const openStatuses = `('pending', 'approved', 'executing', 'unknown')`

// Every transition is a conditional update on the current status and, while
// running, the fence, so a stale caller changes nothing. Times come from the
// store clock.
var confirmationQueries = map[string]string{
	qConfirmInsert: `
INSERT INTO mcp_confirmation
       (id, partner_id, maker_kind, maker_id, client_ref, tool, payload, payload_digest, preview, requirements,
        channel_code, approval_required, expires_at)
VALUES (nextval('mcp_confirmation_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
        CURRENT_TIMESTAMP + CAST(? AS INTEGER) * INTERVAL '1 second')
ON CONFLICT (partner_id, tool, payload_digest) WHERE status_code IN ` + openStatuses + ` DO NOTHING
RETURNING id`,
	qConfirmOpen: `SELECT ` + confirmationColumns + ` FROM mcp_confirmation
 WHERE partner_id = ? AND tool = ? AND payload_digest = ? AND status_code IN ` + openStatuses,
	qConfirmGet: `SELECT ` + confirmationColumns + ` FROM mcp_confirmation WHERE partner_id = ? AND id = ?`,
	qConfirmNotified: `
UPDATE mcp_confirmation SET notification_event_id = ?
 WHERE partner_id = ? AND id = ? AND notification_event_id IS NULL`,
	qConfirmAttach: `
UPDATE mcp_confirmation SET approval_id = ?
 WHERE partner_id = ? AND id = ? AND status_code = 'pending' AND approval_required AND approval_id IS NULL
   AND expires_at > CURRENT_TIMESTAMP
RETURNING id`,
	qConfirmAbandon: `
UPDATE mcp_confirmation SET status_code = 'failed', error_text = ?, completed_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND id = ? AND status_code = 'pending'
RETURNING id`,
	qConfirmDecide: `
UPDATE mcp_confirmation
   SET status_code = ?, decided_at = CURRENT_TIMESTAMP, decider_kind = ?, decider_id = ?, decision_note = ?
 WHERE partner_id = ? AND id = ? AND status_code = 'pending' AND approval_id IS NULL
   AND NOT approval_required
   AND expires_at > CURRENT_TIMESTAMP
RETURNING id`,
	// This runs inside keel's decision transaction.
	qConfirmDecideApproval: `
UPDATE mcp_confirmation
   SET status_code = ?, decided_at = CURRENT_TIMESTAMP, decider_kind = ?, decider_id = ?, decision_note = ?
 WHERE partner_id = ? AND id = ? AND approval_id = ? AND status_code = 'pending'
   AND expires_at > CURRENT_TIMESTAMP
RETURNING id`,
	qConfirmWithdraw: `
UPDATE mcp_confirmation
   SET status_code = 'withdrawn', decided_at = CURRENT_TIMESTAMP, decider_kind = maker_kind, decider_id = maker_id
 WHERE partner_id = ? AND id = ? AND maker_kind = ? AND maker_id = ? AND status_code = 'pending'
RETURNING COALESCE(approval_id, 0)`,
	qConfirmApproved: `
SELECT partner_id, id FROM mcp_confirmation
 WHERE status_code = 'approved'
 ORDER BY decided_at, id
 LIMIT ?`,
	qConfirmClaim: `
UPDATE mcp_confirmation
   SET status_code = 'executing', fence = fence + 1, attempts = attempts + 1,
       lease_until = CURRENT_TIMESTAMP + CAST(? AS INTEGER) * INTERVAL '1 second'
 WHERE partner_id = ? AND id = ? AND status_code = 'approved'
RETURNING fence`,
	qConfirmRenew: `
UPDATE mcp_confirmation SET lease_until = CURRENT_TIMESTAMP + CAST(? AS INTEGER) * INTERVAL '1 second'
 WHERE partner_id = ? AND id = ? AND fence = ? AND status_code = 'executing'
RETURNING id`,
	qConfirmComplete: `
UPDATE mcp_confirmation
   SET status_code = ?, result = ?, error_text = ?, lease_until = NULL,
       completed_at = CASE WHEN ? = 'unknown' THEN NULL ELSE CURRENT_TIMESTAMP END
 WHERE partner_id = ? AND id = ? AND fence = ? AND status_code = 'executing'
RETURNING id`,
	qConfirmLapse: `
UPDATE mcp_confirmation
   SET status_code = 'unknown', lease_until = NULL,
       error_text = 'The run lost its claim before reporting an outcome; reconcile it.'
 WHERE id IN (SELECT id FROM mcp_confirmation
               WHERE status_code = 'executing' AND lease_until <= CURRENT_TIMESTAMP
               ORDER BY lease_until, id LIMIT ? FOR UPDATE SKIP LOCKED)
RETURNING id`,
	qConfirmExpire: `
UPDATE mcp_confirmation SET status_code = 'expired', decided_at = CURRENT_TIMESTAMP
 WHERE id IN (SELECT id FROM mcp_confirmation
               WHERE status_code = 'pending' AND expires_at <= CURRENT_TIMESTAMP
               ORDER BY expires_at, id LIMIT ? FOR UPDATE SKIP LOCKED)
RETURNING id, partner_id, maker_kind, maker_id, tool, COALESCE(approval_id, 0)`,
	qConfirmReconcile: `
UPDATE mcp_confirmation
   SET status_code = ?, reconciled_at = CURRENT_TIMESTAMP, reconciler_kind = ?, reconciler_id = ?,
       reconcile_note = ?, completed_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND id = ? AND status_code = 'unknown'
RETURNING id`,
	// The digest and the decision evidence outlive the payload.
	qConfirmPurge: `
UPDATE mcp_confirmation SET payload = NULL, preview = NULL, result = NULL, purged_at = CURRENT_TIMESTAMP
 WHERE id IN (SELECT id FROM mcp_confirmation
               WHERE status_code IN ('executed', 'failed', 'declined', 'withdrawn', 'expired') AND purged_at IS NULL
                 AND COALESCE(completed_at, decided_at, created_at) < CURRENT_TIMESTAMP - CAST(? AS INTEGER) * INTERVAL '1 second'
               ORDER BY id LIMIT ? FOR UPDATE SKIP LOCKED)
RETURNING id`,
	qConfirmList: `SELECT ` + confirmationColumns + ` FROM mcp_confirmation
 WHERE partner_id = ? AND (status_code IN ` + openStatuses + ` OR created_at >= ?)
 ORDER BY created_at DESC, id DESC
 LIMIT ?`,
}

// TableStore persists the confirmation lifecycle over mcp_confirmation.
type TableStore struct {
	DB keelport.DatabaseRepository
	// NotifyThroughOutbox queues PendingEvent in the transaction that creates an
	// inbox confirmation, so one never exists without its notice.
	NotifyThroughOutbox bool

	once sync.Once
	qs   keelport.QueryService
}

func (s *TableStore) init(ctx context.Context) error {
	if s.DB == nil {
		return fmt.Errorf("%w: confirmation store needs a database", domain.ErrNotReady)
	}
	s.once.Do(func() { s.qs = s.DB.GetQueryService(ctx, confirmationQueries) })
	if s.qs == nil {
		return fmt.Errorf("%w: confirmation store needs a query service", domain.ErrNotReady)
	}
	return nil
}

// pendingNotice is the outbox payload: who prepared which action, never its content.
type pendingNotice struct {
	TenantID       int64     `json:"tenant_id"`
	ConfirmationID int64     `json:"confirmation_id"`
	Tool           string    `json:"tool"`
	MakerKind      string    `json:"maker_kind"`
	MakerID        string    `json:"maker_id"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// Prepare records a new confirmation, or returns the maker's own open one for
// the identical action with created false. Another maker's or client's open
// identical action is ErrConflict: nobody receives someone else's confirmation.
func (s *TableStore) Prepare(ctx context.Context, draft domain.MCPConfirmationDraft) (domain.MCPConfirmation, bool, error) {
	if err := s.init(ctx); err != nil {
		return domain.MCPConfirmation{}, false, err
	}
	requirements, err := ValidateDraft(draft)
	if err != nil {
		return domain.MCPConfirmation{}, false, err
	}
	ctx = context.WithoutCancel(ctx)
	args := []any{draft.TenantID, string(draft.Maker.Kind), draft.Maker.ID, nullable(draft.ClientRef), draft.Tool,
		string(draft.Payload), draft.Digest, string(draft.Preview), string(requirements), string(draft.Channel),
		draft.ApprovalRequired, int64(draft.TTL / time.Second)}
	var id int64
	if s.NotifyThroughOutbox && draft.Channel == domain.MCPConfirmationInbox {
		id, err = s.insertNotified(ctx, draft, args)
	} else {
		id, err = insertID(ctx, s.qs, args)
	}
	if err != nil {
		return domain.MCPConfirmation{}, false, err
	}
	if id > 0 {
		stored, err := s.Get(ctx, domain.MCPConfirmationKey{TenantID: draft.TenantID, ID: id})
		return stored, err == nil, err
	}
	open, err := s.qs.Query(ctx, qConfirmOpen, draft.TenantID, draft.Tool, draft.Digest)
	if err != nil {
		return domain.MCPConfirmation{}, false, fmt.Errorf("read the open confirmation of %s: %w", draft.Tool, err)
	}
	if len(open.Rows) == 0 {
		return domain.MCPConfirmation{}, false, fmt.Errorf("%w: the identical open action closed concurrently; retry", domain.ErrConflict)
	}
	existing := scanConfirmation(open.Rows[0])
	if existing.Maker != draft.Maker || existing.ClientRef != draft.ClientRef {
		return domain.MCPConfirmation{}, false, fmt.Errorf("%w: an identical action is already waiting for another maker", domain.ErrConflict)
	}
	return existing, false, nil
}

func insertID(ctx context.Context, qs keelport.QueryService, args []any) (int64, error) {
	inserted, err := qs.Query(ctx, qConfirmInsert, args...)
	if err != nil {
		return 0, fmt.Errorf("record confirmation: %w", err)
	}
	if len(inserted.Rows) == 0 {
		return 0, nil
	}
	return common.AsInt64(inserted.Rows[0][0]), nil
}

func (s *TableStore) insertNotified(ctx context.Context, draft domain.MCPConfirmationDraft, args []any) (int64, error) {
	queries := maps.Clone(confirmationQueries)
	maps.Copy(queries, outbox.WriteQueries())
	tx, err := s.DB.BeginTx(ctx, queries)
	if err != nil {
		return 0, fmt.Errorf("record confirmation: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	id, err := insertID(ctx, tx, args)
	if err != nil || id == 0 {
		return 0, err
	}
	stored, err := tx.Query(ctx, qConfirmGet, draft.TenantID, id)
	if err != nil {
		return 0, fmt.Errorf("read the new confirmation: %w", err)
	}
	if len(stored.Rows) == 0 {
		return 0, fmt.Errorf("%w: the new confirmation %d", domain.ErrNotFound, id)
	}
	payload, err := json.Marshal(pendingNotice{TenantID: draft.TenantID, ConfirmationID: id, Tool: draft.Tool,
		MakerKind: string(draft.Maker.Kind), MakerID: draft.Maker.ID, ExpiresAt: scanConfirmation(stored.Rows[0]).ExpiresAt})
	if err != nil {
		return 0, fmt.Errorf("encode confirmation notice: %w", err)
	}
	eventID, err := outbox.EnqueueTx(ctx, tx, outbox.Event{PartnerID: draft.TenantID, AggregateType: Aggregate,
		AggregateID: strconv.FormatInt(id, 10), EventType: PendingEvent, Payload: string(payload)})
	if err != nil {
		return 0, fmt.Errorf("queue confirmation notice: %w", err)
	}
	if _, err = tx.Query(ctx, qConfirmNotified, eventID, draft.TenantID, id); err != nil {
		return 0, fmt.Errorf("link confirmation notice: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("record confirmation: commit: %w", err)
	}
	committed = true
	return id, nil
}

// Get reads one confirmation inside its tenant.
func (s *TableStore) Get(ctx context.Context, key domain.MCPConfirmationKey) (domain.MCPConfirmation, error) {
	if err := s.init(ctx); err != nil {
		return domain.MCPConfirmation{}, err
	}
	if key.TenantID <= 0 || key.ID <= 0 {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: confirmation %d", domain.ErrNotFound, key.ID)
	}
	result, err := s.qs.Query(ctx, qConfirmGet, key.TenantID, key.ID)
	if err != nil {
		return domain.MCPConfirmation{}, fmt.Errorf("read confirmation %d: %w", key.ID, err)
	}
	if len(result.Rows) == 0 {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: confirmation %d", domain.ErrNotFound, key.ID)
	}
	return scanConfirmation(result.Rows[0]), nil
}

// AttachApproval hands a pending confirmation to a keel maker-checker request;
// from then on only DecideApprovalTx decides it.
func (s *TableStore) AttachApproval(ctx context.Context, key domain.MCPConfirmationKey, approvalID int64) error {
	if approvalID <= 0 {
		return fmt.Errorf("%w: approval request is required", domain.ErrValidation)
	}
	return s.guarded(ctx, "attach approval to", key, qConfirmAttach, approvalID, key.TenantID, key.ID)
}

// Abandon fails a pending confirmation that can never be decided.
func (s *TableStore) Abandon(ctx context.Context, key domain.MCPConfirmationKey, reason string) error {
	return s.guarded(ctx, "abandon", key, qConfirmAbandon, nullable(truncate(reason)), key.TenantID, key.ID)
}

// Decide records a decision on a confirmation no maker-checker request owns.
// The caller has authorized the decider; Executor.Decide does both.
func (s *TableStore) Decide(ctx context.Context, key domain.MCPConfirmationKey, decider domain.PrincipalRef, approve bool, note string) error {
	if decider.Kind == "" || strings.TrimSpace(decider.ID) == "" {
		return fmt.Errorf("%w: decider is required", domain.ErrPrincipalUnknown)
	}
	return s.guarded(ctx, "decide", key, qConfirmDecide, string(decisionStatus(approve)), string(decider.Kind), decider.ID,
		nullable(truncate(note)), key.TenantID, key.ID)
}

// DecideApprovalTx applies keel's maker-checker decision inside keel's
// transaction; wire it into approval.Service.OnDecided for ApprovalObjectType.
// A confirmation that is no longer pending fails the decision.
func (s *TableStore) DecideApprovalTx(ctx context.Context, tx keelport.TxQueryService, request *approval.Request) error {
	if request == nil || request.ObjectType != ApprovalObjectType {
		return fmt.Errorf("%w: not a confirmation approval", domain.ErrValidation)
	}
	var approve bool
	switch request.Status {
	case approval.StatusApproved:
		approve = true
	case approval.StatusRejected:
	default:
		return fmt.Errorf("%w: approval status %q is not a decision", domain.ErrValidation, request.Status)
	}
	catalog, ok := tx.(keelport.TxQueryCatalog)
	if !ok {
		return fmt.Errorf("%w: the approval transaction cannot bind the confirmation query catalog", domain.ErrNotReady)
	}
	qs := catalog.QueryService("scout.confirmation", confirmationQueries)
	if qs == nil {
		return fmt.Errorf("%w: the approval transaction returned no confirmation query service", domain.ErrNotReady)
	}
	decided, err := qs.Query(ctx, qConfirmDecideApproval,
		string(decisionStatus(approve)), string(domain.PrincipalHuman), strconv.FormatInt(request.CheckerID, 10),
		nullable(truncate(request.DecisionNote)), request.PartnerID, request.ObjectID, request.ID)
	if err != nil {
		return fmt.Errorf("decide confirmation %d: %w", request.ObjectID, err)
	}
	if len(decided.Rows) == 0 {
		return fmt.Errorf("%w: confirmation %d is not pending under approval %d", domain.ErrConflict, request.ObjectID, request.ID)
	}
	return nil
}

func decisionStatus(approve bool) domain.MCPConfirmationStatus {
	if approve {
		return domain.MCPConfirmationApproved
	}
	return domain.MCPConfirmationDeclined
}

// Withdraw closes the maker's own pending confirmation and returns its keel
// approval id, which the caller withdraws; one left open is closed by expiry.
func (s *TableStore) Withdraw(ctx context.Context, key domain.MCPConfirmationKey, maker domain.PrincipalRef) (int64, error) {
	if err := s.init(ctx); err != nil {
		return 0, err
	}
	withdrawn, err := s.qs.Query(context.WithoutCancel(ctx), qConfirmWithdraw, key.TenantID, key.ID, string(maker.Kind), maker.ID)
	if err != nil {
		return 0, fmt.Errorf("withdraw confirmation %d: %w", key.ID, err)
	}
	if len(withdrawn.Rows) > 0 {
		return common.AsInt64(withdrawn.Rows[0][0]), nil
	}
	stored, err := s.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if stored.Maker != maker {
		return 0, fmt.Errorf("%w: confirmation %d", domain.ErrNotFound, key.ID)
	}
	return 0, fmt.Errorf("%w: confirmation %d is %s", domain.ErrConflict, key.ID, stored.Status)
}

// Approved lists approved confirmations waiting to run, oldest decision first.
func (s *TableStore) Approved(ctx context.Context, limit int) ([]domain.MCPConfirmationKey, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	result, err := s.qs.Query(ctx, qConfirmApproved, batch(limit))
	if err != nil {
		return nil, fmt.Errorf("list approved confirmations: %w", err)
	}
	keys := make([]domain.MCPConfirmationKey, 0, len(result.Rows))
	for _, row := range result.Rows {
		keys = append(keys, domain.MCPConfirmationKey{TenantID: common.AsInt64(row[0]), ID: common.AsInt64(row[1])})
	}
	return keys, nil
}

// Claim moves an approved confirmation to executing under a new fence; ok is
// false when it was not approved, including when another executor claimed it.
func (s *TableStore) Claim(ctx context.Context, key domain.MCPConfirmationKey, lease time.Duration) (int64, bool, error) {
	if err := s.init(ctx); err != nil {
		return 0, false, err
	}
	seconds, err := leaseSeconds(lease)
	if err != nil {
		return 0, false, err
	}
	claimed, err := s.qs.Query(context.WithoutCancel(ctx), qConfirmClaim, seconds, key.TenantID, key.ID)
	if err != nil {
		return 0, false, fmt.Errorf("claim confirmation %d: %w", key.ID, err)
	}
	if len(claimed.Rows) == 0 {
		return 0, false, nil
	}
	return common.AsInt64(claimed.Rows[0][0]), true, nil
}

// Renew extends a live claim; ErrConflict means the claim was lost.
func (s *TableStore) Renew(ctx context.Context, key domain.MCPConfirmationKey, fence int64, lease time.Duration) error {
	seconds, err := leaseSeconds(lease)
	if err != nil {
		return err
	}
	return s.guarded(ctx, "renew the claim on", key, qConfirmRenew, seconds, key.TenantID, key.ID, fence)
}

// Complete records the outcome of a claim whose fence still holds: executed,
// failed, or unknown. reason is shown to people and must be client-safe.
func (s *TableStore) Complete(ctx context.Context, key domain.MCPConfirmationKey, fence int64, status domain.MCPConfirmationStatus, result json.RawMessage, reason string) error {
	switch status {
	case domain.MCPConfirmationExecuted, domain.MCPConfirmationFailed, domain.MCPConfirmationUnknown:
	default:
		return fmt.Errorf("%w: %q is not an outcome", domain.ErrValidation, status)
	}
	var stored any
	if len(result) > 0 {
		if len(result) > MaxDocumentBytes || !json.Valid(result) {
			return fmt.Errorf("%w: result must be JSON of at most %d bytes", domain.ErrValidation, MaxDocumentBytes)
		}
		stored = string(result)
	}
	return s.guarded(ctx, "complete", key, qConfirmComplete, string(status), stored, nullable(truncate(reason)),
		string(status), key.TenantID, key.ID, fence)
}

// MarkLapsed turns a bounded batch of expired claims unknown. They are never run again.
func (s *TableStore) MarkLapsed(ctx context.Context, limit int) (int, error) {
	if err := s.init(ctx); err != nil {
		return 0, err
	}
	lapsed, err := s.qs.Query(context.WithoutCancel(ctx), qConfirmLapse, batch(limit))
	if err != nil {
		return 0, fmt.Errorf("mark lapsed confirmations unknown: %w", err)
	}
	return len(lapsed.Rows), nil
}

// ExpireDue closes a bounded batch of pending confirmations past their expiry.
func (s *TableStore) ExpireDue(ctx context.Context, limit int) ([]domain.ExpiredMCPConfirmation, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	expired, err := s.qs.Query(context.WithoutCancel(ctx), qConfirmExpire, batch(limit))
	if err != nil {
		return nil, fmt.Errorf("expire confirmations: %w", err)
	}
	out := make([]domain.ExpiredMCPConfirmation, 0, len(expired.Rows))
	for _, row := range expired.Rows {
		out = append(out, domain.ExpiredMCPConfirmation{
			MCPConfirmationKey: domain.MCPConfirmationKey{ID: common.AsInt64(row[0]), TenantID: common.AsInt64(row[1])},
			Maker:              domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[2])), ID: common.AsString(row[3])},
			Tool:               common.AsString(row[4]), ApprovalID: common.AsInt64(row[5]),
		})
	}
	return out, nil
}

// Reconcile records what a person verified about an unknown outcome. It never
// runs the action. The caller has authorized the reconciler; Executor.Reconcile does both.
func (s *TableStore) Reconcile(ctx context.Context, key domain.MCPConfirmationKey, reconciler domain.PrincipalRef, outcome domain.MCPConfirmationStatus, note string) error {
	note = strings.TrimSpace(note)
	switch {
	case outcome != domain.MCPConfirmationExecuted && outcome != domain.MCPConfirmationFailed:
		return fmt.Errorf("%w: a reconciled outcome is executed or failed", domain.ErrValidation)
	case note == "" || utf8.RuneCountInString(note) > MaxNoteRunes:
		return fmt.Errorf("%w: a reconciliation note is 1 to %d characters", domain.ErrValidation, MaxNoteRunes)
	case reconciler.Kind == "" || strings.TrimSpace(reconciler.ID) == "":
		return fmt.Errorf("%w: reconciler is required", domain.ErrPrincipalUnknown)
	}
	return s.guarded(ctx, "reconcile", key, qConfirmReconcile, string(outcome), string(reconciler.Kind), reconciler.ID,
		note, key.TenantID, key.ID)
}

// Purge drops the payload, preview, and result of a bounded batch of
// confirmations finished longer ago than retention.
func (s *TableStore) Purge(ctx context.Context, retention time.Duration, limit int) (int, error) {
	if err := s.init(ctx); err != nil {
		return 0, err
	}
	if retention < 24*time.Hour {
		return 0, fmt.Errorf("%w: retention must be at least a day", domain.ErrValidation)
	}
	purged, err := s.qs.Query(context.WithoutCancel(ctx), qConfirmPurge, int64(retention/time.Second), batch(limit))
	if err != nil {
		return 0, fmt.Errorf("purge confirmations: %w", err)
	}
	return len(purged.Rows), nil
}

// List returns a tenant's open confirmations and those created since, newest first.
func (s *TableStore) List(ctx context.Context, tenantID int64, since time.Time, limit int) ([]domain.MCPConfirmation, error) {
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	if tenantID <= 0 {
		return nil, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	}
	listed, err := s.qs.Query(ctx, qConfirmList, tenantID, since, batch(limit))
	if err != nil {
		return nil, fmt.Errorf("list confirmations: %w", err)
	}
	out := make([]domain.MCPConfirmation, 0, len(listed.Rows))
	for _, row := range listed.Rows {
		out = append(out, scanConfirmation(row))
	}
	return out, nil
}

// guarded runs a conditional update; no returned row means the guard refused it.
func (s *TableStore) guarded(ctx context.Context, verb string, key domain.MCPConfirmationKey, query string, args ...any) error {
	if err := s.init(ctx); err != nil {
		return err
	}
	updated, err := s.qs.Query(context.WithoutCancel(ctx), query, args...)
	if err != nil {
		return fmt.Errorf("%s confirmation %d: %w", verb, key.ID, err)
	}
	if len(updated.Rows) == 0 {
		return fmt.Errorf("%w: cannot %s confirmation %d in its current state", domain.ErrConflict, verb, key.ID)
	}
	return nil
}

// ValidateDraft checks a draft as Prepare does and returns its requirements,
// defaulted to an empty object.
func ValidateDraft(draft domain.MCPConfirmationDraft) (json.RawMessage, error) {
	requirements := draft.Requirements
	if len(requirements) == 0 {
		requirements = json.RawMessage(`{}`)
	}
	switch {
	case draft.TenantID <= 0:
		return nil, fmt.Errorf("%w: tenant is required", domain.ErrValidation)
	case draft.Maker.Kind == "" || strings.TrimSpace(draft.Maker.ID) == "":
		return nil, fmt.Errorf("%w: maker is required", domain.ErrPrincipalUnknown)
	case strings.TrimSpace(draft.Tool) == "":
		return nil, fmt.Errorf("%w: tool is required", domain.ErrValidation)
	case !isDigest(draft.Digest):
		return nil, fmt.Errorf("%w: digest must be 64 lowercase hex characters", domain.ErrValidation)
	case draft.Channel != domain.MCPConfirmationElicitation && draft.Channel != domain.MCPConfirmationInbox:
		return nil, fmt.Errorf("%w: channel %q", domain.ErrValidation, draft.Channel)
	case draft.ApprovalRequired && draft.Channel != domain.MCPConfirmationInbox:
		return nil, fmt.Errorf("%w: maker-checker approval requires the inbox channel", domain.ErrValidation)
	case draft.TTL < time.Second || draft.TTL/time.Second > 1<<31-1:
		return nil, fmt.Errorf("%w: expiry must be between a second and %d seconds", domain.ErrValidation, 1<<31-1)
	}
	for name, document := range map[string]json.RawMessage{"payload": draft.Payload, "preview": draft.Preview, "requirements": requirements} {
		if len(document) == 0 || len(document) > MaxDocumentBytes || !json.Valid(document) {
			return nil, fmt.Errorf("%w: %s must be JSON of at most %d bytes", domain.ErrValidation, name, MaxDocumentBytes)
		}
	}
	return requirements, nil
}

// ActionDigest identifies an action by its tool and canonical arguments.
func ActionDigest(tool string, canonicalArguments []byte) string {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), canonicalArguments...))
	return hex.EncodeToString(sum[:])
}

func isDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func leaseSeconds(lease time.Duration) (int64, error) {
	if lease < time.Second || lease/time.Second > 1<<31-1 {
		return 0, fmt.Errorf("%w: lease must be between a second and %d seconds", domain.ErrValidation, 1<<31-1)
	}
	return int64(lease / time.Second), nil
}

func batch(limit int) int {
	switch {
	case limit <= 0:
		return DefaultBatch
	case limit > MaxBatch:
		return MaxBatch
	}
	return limit
}

func truncate(note string) string {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) <= MaxNoteRunes {
		return note
	}
	return string([]rune(note)[:MaxNoteRunes])
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func scanConfirmation(row []any) domain.MCPConfirmation {
	decided, _ := common.AsTimeOK(row[16])
	completed, _ := common.AsTimeOK(row[24])
	return domain.MCPConfirmation{
		ID: common.AsInt64(row[0]), TenantID: common.AsInt64(row[1]),
		Maker:     domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[2])), ID: common.AsString(row[3])},
		ClientRef: common.AsString(row[4]), Tool: common.AsString(row[5]), Payload: rawJSON(row[6]),
		PayloadDigest: common.AsString(row[7]), Preview: rawJSON(row[8]), Requirements: rawJSON(row[9]),
		Channel: domain.MCPConfirmationChannel(common.AsString(row[10])), Status: domain.MCPConfirmationStatus(common.AsString(row[11])),
		ApprovalRequired: common.AsBool(row[12]), ApprovalID: common.AsInt64(row[13]),
		ExpiresAt: common.AsTime(row[14]), CreatedAt: common.AsTime(row[15]), DecidedAt: decided,
		Decider:      domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[17])), ID: common.AsString(row[18])},
		DecisionNote: common.AsString(row[19]), Fence: common.AsInt64(row[20]), Attempts: common.AsInt64(row[21]),
		Result: rawJSON(row[22]), Error: common.AsString(row[23]), CompletedAt: completed,
		Reconciler:    domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[25])), ID: common.AsString(row[26])},
		ReconcileNote: common.AsString(row[27]), Expired: common.AsBool(row[28]),
	}
}

func rawJSON(value any) json.RawMessage {
	text := common.AsString(value)
	if text == "" {
		return nil
	}
	return json.RawMessage(text)
}

var _ contract.MCPConfirmationStore = (*TableStore)(nil)
