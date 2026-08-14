package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	keelcommon "github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Turn-ledger sentinels; consumers map them to their transport statuses.
var (
	ErrTurnInFlight = errors.New("an identical request is already running")
	ErrTurnFenced   = errors.New("turn execution was superseded by a newer attempt")
	ErrTurnConflict = errors.New("request id was already used with different input")
	ErrTurnFailed   = errors.New("the prior execution of this request failed")
)

const (
	qLedgerFindTurn           = "scout_ledger_find_turn"
	qLedgerEnsureConversation = "scout_ledger_ensure_conversation"
	qLedgerGetConversation    = "scout_ledger_get_conversation"
	qLedgerLock               = "scout_ledger_lock"
	qLedgerInsertTurn         = "scout_ledger_insert_turn"
	qLedgerInsertDetail       = "scout_ledger_insert_detail"
	qLedgerAttachJob          = "scout_ledger_attach_job"
	qLedgerActivate           = "scout_ledger_activate"
	qLedgerStageSuccess       = "scout_ledger_stage_success"
	qLedgerStageFailure       = "scout_ledger_stage_failure"
	qLedgerFinishSuccess      = "scout_ledger_finish_success"
	qLedgerFinishFailure      = "scout_ledger_finish_failure"
	qLedgerFailUnreserved     = "scout_ledger_fail_unreserved"
	qLedgerInsertUsageEvent   = "scout_ledger_insert_usage_event"
	qLedgerSetJobStatus       = "scout_ledger_set_job_status"
	qLedgerActivateJob        = "scout_ledger_activate_job"
	qLedgerStageJobUsage      = "scout_ledger_stage_job_usage"
	qLedgerAttachJobArtifact  = "scout_ledger_attach_job_artifact"
	qLedgerCompleteJobTurn    = "scout_ledger_complete_job_turn"
)

var turnLedgerQueries = map[string]string{
	qLedgerFindTurn: `
SELECT runtime.tenant_id, runtime.conversation_id, runtime.turn_no, runtime.request_id,
       runtime.status_code, runtime.input_digest, runtime.response_digest,
       detail.task_kind, detail.input_summary, detail.result_kind, detail.result_payload,
       detail.error_text, detail.job_ref, detail.artifact_ref, detail.release_digest,
       detail.active_reservation_id,
       detail.staged_input_tokens, detail.staged_output_tokens,
       detail.staged_cost_minor_units, detail.staged_currency_code,
       reservation.status_code, reservation.attempt_no,
       COALESCE(reservation.expires_at <= CURRENT_TIMESTAMP, FALSE),
       conversation.agent_id, conversation.agent_version, runtime.queued_at
  FROM conversation_turn runtime
  JOIN agent_conversation conversation
    ON conversation.tenant_id = runtime.tenant_id
   AND conversation.conversation_id = runtime.conversation_id
  JOIN conversation_turn_detail detail
    ON detail.tenant_id = runtime.tenant_id
   AND detail.conversation_id = runtime.conversation_id
   AND detail.turn_no = runtime.turn_no
  LEFT JOIN budget_reservation reservation
    ON reservation.tenant_id = detail.tenant_id
   AND reservation.reservation_id = detail.active_reservation_id
 WHERE runtime.tenant_id = ? AND runtime.request_id = ?`,

	// Serialized by the ledger's advisory workspace lock. Deliberately no
	// uniqueness rule on agent_conversation itself.
	qLedgerEnsureConversation: `
INSERT INTO agent_conversation
       (tenant_id, conversation_id, agent_id, agent_version, end_user_ref)
SELECT ?, ?, ?, ?, ?
 WHERE NOT EXISTS (
       SELECT 1
         FROM agent_conversation
        WHERE tenant_id = ? AND end_user_ref = ? AND agent_id = ? AND agent_version = ?
          AND closed_at IS NULL
 )`,

	qLedgerGetConversation: `
SELECT conversation_id
 FROM agent_conversation
 WHERE tenant_id = ? AND end_user_ref = ? AND agent_id = ? AND agent_version = ?
   AND closed_at IS NULL
 ORDER BY created_at, conversation_id
 LIMIT 1`,

	qLedgerLock: `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`,

	qLedgerInsertTurn: `
INSERT INTO conversation_turn
       (tenant_id, conversation_id, turn_no, request_id, status_code,
        input_uri, input_digest, started_at)
SELECT ?, ?, COALESCE(MAX(turn_no), 0) + 1, ?, ?, ?, ?,
       CASE WHEN ? = 'queued' THEN NULL ELSE CURRENT_TIMESTAMP END
  FROM conversation_turn
 WHERE tenant_id = ? AND conversation_id = ?
ON CONFLICT DO NOTHING
RETURNING turn_no`,

	qLedgerInsertDetail: `
INSERT INTO conversation_turn_detail
       (tenant_id, conversation_id, turn_no, task_kind, input_summary, release_digest)
VALUES (?, ?, ?, ?, ?, ?)`,

	qLedgerAttachJob: `
UPDATE conversation_turn_detail
   SET result_kind = ?, job_ref = ?
 WHERE tenant_id = ? AND conversation_id = ? AND turn_no = ? AND job_ref IS NULL
RETURNING turn_no`,

	// The detail row is the single execution claim. Compare-and-swap prevents
	// two callers that replay the same live reservation from both invoking the
	// provider.
	qLedgerActivate: `
UPDATE conversation_turn_detail detail
   SET active_reservation_id = ?
  FROM conversation_turn runtime
 WHERE detail.tenant_id = ? AND detail.conversation_id = ? AND detail.turn_no = ?
   AND runtime.tenant_id = detail.tenant_id
   AND runtime.conversation_id = detail.conversation_id
   AND runtime.turn_no = detail.turn_no
   AND runtime.status_code IN ('queued', 'running', 'streaming')
   AND detail.active_reservation_id IS NOT DISTINCT FROM ?
   AND EXISTS (SELECT 1 FROM budget_reservation reservation
                WHERE reservation.tenant_id = detail.tenant_id
                  AND reservation.reservation_id = ?
                  AND reservation.request_id = runtime.request_id
                  AND reservation.attempt_no = ?
                  AND reservation.status_code = 'held'
                  AND reservation.expires_at > CURRENT_TIMESTAMP)
RETURNING detail.turn_no`,

	qLedgerStageSuccess: `
UPDATE conversation_turn_detail detail
   SET result_kind = ?, result_payload = ?, error_text = NULL,
       staged_input_tokens = ?, staged_output_tokens = ?,
       staged_cost_minor_units = ?, staged_currency_code = ?
  FROM conversation_turn runtime
 WHERE detail.tenant_id = ? AND detail.conversation_id = ? AND detail.turn_no = ?
   AND runtime.tenant_id = detail.tenant_id
   AND runtime.conversation_id = detail.conversation_id
   AND runtime.turn_no = detail.turn_no
   AND runtime.status_code IN ('running', 'streaming')
   AND detail.active_reservation_id = ?
   AND detail.result_payload IS NULL
   AND EXISTS (SELECT 1 FROM budget_reservation reservation
                WHERE reservation.tenant_id = detail.tenant_id
                  AND reservation.reservation_id = detail.active_reservation_id
                  AND reservation.status_code = 'held'
                  AND reservation.expires_at > CURRENT_TIMESTAMP)
RETURNING detail.turn_no`,

	qLedgerStageFailure: `
UPDATE conversation_turn_detail detail
   SET error_text = ?
  FROM conversation_turn runtime
 WHERE detail.tenant_id = ? AND detail.conversation_id = ? AND detail.turn_no = ?
   AND runtime.tenant_id = detail.tenant_id
   AND runtime.conversation_id = detail.conversation_id
   AND runtime.turn_no = detail.turn_no
   AND runtime.status_code IN ('running', 'streaming')
   AND detail.active_reservation_id = ?
   AND EXISTS (SELECT 1 FROM budget_reservation reservation
                WHERE reservation.tenant_id = detail.tenant_id
                  AND reservation.reservation_id = detail.active_reservation_id
                  AND reservation.status_code = 'held'
                  AND reservation.expires_at > CURRENT_TIMESTAMP)
RETURNING detail.turn_no`,

	qLedgerFinishSuccess: `
UPDATE conversation_turn runtime
   SET status_code = 'completed', response_uri = ?, response_digest = ?,
       completed_at = CURRENT_TIMESTAMP
  FROM conversation_turn_detail detail
 WHERE runtime.tenant_id = ? AND runtime.conversation_id = ? AND runtime.turn_no = ?
   AND detail.tenant_id = runtime.tenant_id
   AND detail.conversation_id = runtime.conversation_id
   AND detail.turn_no = runtime.turn_no
   AND runtime.status_code IN ('running', 'streaming')
   AND detail.active_reservation_id = ?
   AND EXISTS (SELECT 1 FROM budget_reservation reservation
                WHERE reservation.tenant_id = runtime.tenant_id
                  AND reservation.reservation_id = detail.active_reservation_id
                  AND reservation.status_code = 'settled'
                  AND reservation.settled_tokens = detail.staged_input_tokens + detail.staged_output_tokens
                  AND reservation.settled_cost_minor_units = detail.staged_cost_minor_units)
RETURNING runtime.turn_no`,

	qLedgerFinishFailure: `
UPDATE conversation_turn runtime
   SET status_code = 'failed', completed_at = CURRENT_TIMESTAMP
  FROM conversation_turn_detail detail
 WHERE runtime.tenant_id = ? AND runtime.conversation_id = ? AND runtime.turn_no = ?
   AND detail.tenant_id = runtime.tenant_id
   AND detail.conversation_id = runtime.conversation_id
   AND detail.turn_no = runtime.turn_no
   AND runtime.status_code IN ('running', 'streaming')
   AND detail.active_reservation_id = ?
   AND EXISTS (SELECT 1 FROM budget_reservation reservation
                WHERE reservation.tenant_id = runtime.tenant_id
                  AND reservation.reservation_id = detail.active_reservation_id
                  AND reservation.status_code = 'released')
RETURNING runtime.turn_no`,

	qLedgerFailUnreserved: `
WITH failed AS (
  UPDATE conversation_turn
     SET status_code = 'failed', started_at = COALESCE(started_at, CURRENT_TIMESTAMP),
         completed_at = CURRENT_TIMESTAMP
   WHERE tenant_id = ? AND conversation_id = ? AND turn_no = ?
     AND status_code IN ('queued', 'running')
  RETURNING tenant_id, conversation_id, turn_no
)
UPDATE conversation_turn_detail detail
   SET error_text = ?
  FROM failed
 WHERE detail.tenant_id = failed.tenant_id
   AND detail.conversation_id = failed.conversation_id
   AND detail.turn_no = failed.turn_no
RETURNING detail.turn_no`,

	// Idempotent per (turn, category); a replayed finalization is a no-op.
	qLedgerInsertUsageEvent: `
INSERT INTO usage_event
       (id, tenant_id, conversation_id, turn_no, category_code, subject_ref,
        input_tokens, output_tokens, cost_minor_units, currency_code)
SELECT nextval('usage_event_seq'), ?, ?, ?, ?, ?, ?, ?, ?, ?
 WHERE NOT EXISTS (SELECT 1 FROM usage_event
                    WHERE tenant_id = ? AND conversation_id = ? AND turn_no = ? AND category_code = ?)
RETURNING id`,

	// Job-keyed variants for workers that execute queued turns attached to an
	// external job reference. Compose them inside the worker's own transaction
	// by merging Queries() into its query map.
	qLedgerSetJobStatus: `
UPDATE conversation_turn runtime
   SET status_code = ?,
       started_at = CASE WHEN ? = 'running' THEN COALESCE(started_at, CURRENT_TIMESTAMP) ELSE started_at END,
       completed_at = CASE WHEN ? = 'failed' THEN CURRENT_TIMESTAMP ELSE NULL END
  FROM conversation_turn_detail detail
 WHERE detail.tenant_id = ? AND detail.job_ref = ? AND detail.task_kind = ?
   AND runtime.tenant_id = detail.tenant_id
   AND runtime.conversation_id = detail.conversation_id
   AND runtime.turn_no = detail.turn_no
   AND runtime.status_code IN ('queued', 'running')
RETURNING runtime.turn_no`,

	qLedgerActivateJob: `
UPDATE conversation_turn_detail detail
   SET active_reservation_id = ?
  FROM conversation_turn runtime
 WHERE detail.tenant_id = ? AND detail.job_ref = ? AND detail.task_kind = ?
   AND runtime.tenant_id = detail.tenant_id
   AND runtime.conversation_id = detail.conversation_id
   AND runtime.turn_no = detail.turn_no
   AND runtime.status_code = 'running'
   AND (detail.active_reservation_id IS NULL
        OR detail.active_reservation_id = ?
        OR EXISTS (SELECT 1 FROM budget_reservation old
                    WHERE old.tenant_id = detail.tenant_id
                      AND old.reservation_id = detail.active_reservation_id
                      AND old.status_code = 'expired'))
   AND EXISTS (SELECT 1 FROM budget_reservation current
                WHERE current.tenant_id = detail.tenant_id
                  AND current.reservation_id = ?
                  AND current.status_code = 'held'
                  AND current.expires_at > CURRENT_TIMESTAMP)
RETURNING detail.conversation_id, detail.turn_no`,

	qLedgerStageJobUsage: `
UPDATE conversation_turn_detail detail
   SET staged_input_tokens = ?, staged_output_tokens = ?,
       staged_cost_minor_units = ?, staged_currency_code = ?
  FROM conversation_turn runtime
 WHERE detail.tenant_id = ? AND detail.job_ref = ? AND detail.task_kind = ?
   AND runtime.tenant_id = detail.tenant_id
   AND runtime.conversation_id = detail.conversation_id
   AND runtime.turn_no = detail.turn_no
   AND runtime.status_code = 'running'
   AND detail.active_reservation_id = ?
   AND EXISTS (SELECT 1 FROM budget_reservation reservation
                WHERE reservation.tenant_id = detail.tenant_id
                  AND reservation.reservation_id = detail.active_reservation_id
                  AND reservation.status_code = 'held'
                  AND reservation.expires_at > CURRENT_TIMESTAMP)
RETURNING detail.turn_no`,

	qLedgerAttachJobArtifact: `
UPDATE conversation_turn_detail
   SET artifact_ref = ?
 WHERE tenant_id = ? AND job_ref = ? AND task_kind = ?
   AND active_reservation_id = ? AND artifact_ref IS NULL
RETURNING turn_no`,

	// Finalization is fenced by the settled reservation matching staged usage.
	qLedgerCompleteJobTurn: `
UPDATE conversation_turn runtime
   SET status_code = 'completed', started_at = COALESCE(started_at, CURRENT_TIMESTAMP),
       response_uri = ?, response_digest = ?, completed_at = CURRENT_TIMESTAMP
  FROM conversation_turn_detail detail
 WHERE detail.tenant_id = ? AND detail.job_ref = ? AND detail.task_kind = ?
   AND runtime.tenant_id = detail.tenant_id
   AND runtime.conversation_id = detail.conversation_id
   AND runtime.turn_no = detail.turn_no
   AND runtime.status_code = 'running'
   AND detail.active_reservation_id = ?
   AND EXISTS (SELECT 1 FROM budget_reservation reservation
                WHERE reservation.tenant_id = detail.tenant_id
                  AND reservation.reservation_id = detail.active_reservation_id
                  AND reservation.status_code = 'settled'
                  AND reservation.settled_tokens = detail.staged_input_tokens + detail.staged_output_tokens
                  AND reservation.settled_cost_minor_units = detail.staged_cost_minor_units)
RETURNING runtime.turn_no`,
}

// TurnLedgerQueries returns the ledger's named-SQL map, for workers that
// compose job-keyed transitions inside their own transactions.
func TurnLedgerQueries() map[string]string {
	return keelcommon.MergeMaps(turnLedgerQueries)
}

// TurnState is one governed turn joined with its reservation lease.
type TurnState struct {
	TenantID           int64
	ConversationID     string
	TurnNo             int64
	RequestID          string
	Status             string
	InputDigest        string
	ResponseDigest     string
	TaskKind           string
	InputSummary       string
	ResultKind         string
	ResultPayload      string
	ErrorText          string
	JobRef             int64
	ArtifactRef        int64
	ReleaseDigest      string
	ActiveReservation  string
	InputTokens        int64
	OutputTokens       int64
	CostMinorUnits     int64
	Currency           string
	ReservationStatus  string
	ReservationAttempt int64
	ReservationExpired bool
	AgentID            string
	AgentVersion       string
	QueuedAt           time.Time
}

// TurnExecution is a live claim: the turn, its held reservation, the model.
type TurnExecution struct {
	Turn        TurnState
	Reservation domain.BudgetReservation
	Model       domain.ModelReference
}

// TurnLedger drives the durable request/response turn lifecycle for a
// conversation agent over the runtime schema: one workspace conversation per
// (tenant, end user, release), idempotent turns by request id, budget
// reservation fencing, staged results, and exactly-once settlement with one
// usage event per settled turn.
type TurnLedger struct {
	DB     keelport.DatabaseRepository
	Budget contract.TenantBudgetManager
	// Namespace scopes advisory lock keys and turn URIs, e.g. "workspace".
	Namespace string
	// Currency is the model-catalog denomination every quote must price in.
	Currency string
	// UsageCategory labels the usage event written per settled turn.
	UsageCategory string
	// Activity records last-run attribution after a completed turn; nil skips.
	Activity contract.AgentRunRecorder
	// Metrics records turn latency/usage dimensions; nil skips.
	Metrics BaseRecorder
	// OnSettled runs inside the finalization transaction, after the
	// usage-event insert, for consumer-side accounting mirrors.
	OnSettled func(ctx context.Context, qs keelport.QueryService, turn TurnState, usage domain.Usage) error

	once sync.Once
	qs   keelport.QueryService
}

func (l *TurnLedger) queries(ctx context.Context) keelport.QueryService {
	l.once.Do(func() { l.qs = l.DB.GetQueryService(ctx, turnLedgerQueries) })
	return l.qs
}

func (l *TurnLedger) inputURI(requestID string) string {
	return fmt.Sprintf("scout://%s/input/%s", l.Namespace, requestID)
}

func (l *TurnLedger) resultURI(requestID string) string {
	return fmt.Sprintf("scout://%s/result/%s", l.Namespace, requestID)
}

// NormalizeRequestID bounds a client idempotency key; empty gets a fresh id.
func (l *TurnLedger) NormalizeRequestID(ctx context.Context, requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return fmt.Sprintf("%s-%d", l.Namespace, l.queries(ctx).GenID())
	}
	return keelcommon.TruncateRunes(requestID, 64)
}

// DigestStrings canonicalizes turn input for the idempotency conflict check.
func DigestStrings(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// QuoteUsage estimates reservation-worthy usage, priced on the agent's model.
func QuoteUsage(ctx context.Context, agent contract.PricedAgent, inputText string) (domain.Usage, error) {
	inputTokens := int64(len([]rune(inputText))/4) + 1000
	if inputTokens < 1000 {
		inputTokens = 1000
	}
	modelUsage := domain.ModelUsage{InputTokens: inputTokens, OutputTokens: 2000}
	cost, currency, err := agent.Cost(ctx, modelUsage)
	if err != nil {
		return domain.Usage{}, err
	}
	return domain.Usage{
		InputTokens: inputTokens, OutputTokens: 2000,
		CostMinorUnits: cost, Currency: currency,
	}, nil
}

// PriceUsage prices actual usage; work that ran never costs zero.
func PriceUsage(ctx context.Context, agent contract.PricedAgent, inputTokens, outputTokens int64) (domain.Usage, error) {
	cost, currency, err := agent.Cost(ctx, domain.ModelUsage{InputTokens: inputTokens, OutputTokens: outputTokens})
	if err != nil {
		return domain.Usage{}, err
	}
	if cost < 1 {
		cost = 1
	}
	return domain.Usage{
		InputTokens: inputTokens, OutputTokens: outputTokens,
		CostMinorUnits: cost, Currency: currency,
	}, nil
}

func (l *TurnLedger) findTurn(ctx context.Context, tenantID int64, requestID string) (TurnState, bool, error) {
	result, err := l.queries(ctx).Query(ctx, qLedgerFindTurn, tenantID, requestID)
	if err != nil {
		return TurnState{}, false, err
	}
	if len(result.Rows) == 0 {
		return TurnState{}, false, nil
	}
	row := result.Rows[0]
	if len(row) < 26 {
		return TurnState{}, false, fmt.Errorf("decode turn: expected 26 columns, got %d", len(row))
	}
	state := TurnState{
		TenantID: keelcommon.AsInt64(row[0]), ConversationID: keelcommon.AsString(row[1]),
		TurnNo: keelcommon.AsInt64(row[2]), RequestID: keelcommon.AsString(row[3]),
		Status: keelcommon.AsString(row[4]), InputDigest: keelcommon.AsString(row[5]),
		ResponseDigest: keelcommon.AsString(row[6]), TaskKind: keelcommon.AsString(row[7]),
		InputSummary: keelcommon.AsString(row[8]), ResultKind: keelcommon.AsString(row[9]),
		ResultPayload: keelcommon.AsString(row[10]), ErrorText: keelcommon.AsString(row[11]),
		JobRef: optionalInt64(row[12]), ArtifactRef: optionalInt64(row[13]),
		ReleaseDigest: keelcommon.AsString(row[14]), ActiveReservation: keelcommon.AsString(row[15]),
		InputTokens: optionalInt64(row[16]), OutputTokens: optionalInt64(row[17]),
		CostMinorUnits: optionalInt64(row[18]), Currency: keelcommon.AsString(row[19]),
		ReservationStatus: keelcommon.AsString(row[20]), ReservationAttempt: optionalInt64(row[21]),
		ReservationExpired: keelcommon.AsBool(row[22]),
		AgentID:            keelcommon.AsString(row[23]), AgentVersion: keelcommon.AsString(row[24]),
	}
	if queuedAt, ok := row[25].(time.Time); ok {
		state.QueuedAt = queuedAt
	}
	return state, true, nil
}

// Inspect resolves a request id: terminal turns replay their stored outcome,
// live ones report ErrTurnInFlight, interrupted ones are finished first.
func (l *TurnLedger) Inspect(ctx context.Context, tenantID int64, requestID, taskKind, inputDigest string) (TurnState, any, bool, error) {
	state, found, err := l.findTurn(ctx, tenantID, requestID)
	if err != nil || !found {
		return state, nil, false, err
	}
	if state.TaskKind != taskKind || state.InputDigest != inputDigest {
		return state, nil, false, ErrTurnConflict
	}
	switch state.Status {
	case "completed":
		if state.ResultPayload == "" {
			return state, nil, true, nil
		}
		return state, json.RawMessage(state.ResultPayload), true, nil
	case "failed", "cancelled":
		return state, nil, true, storedTurnError(state)
	}
	if state.ResultPayload != "" && state.ReservationStatus == "settled" {
		if err := l.finishSuccessful(ctx, state, domain.BudgetReservation{
			TenantID: state.TenantID, ReservationID: state.ActiveReservation,
			RequestID: state.RequestID, Attempt: state.ReservationAttempt,
		}); err != nil {
			return state, nil, false, err
		}
		return state, json.RawMessage(state.ResultPayload), true, nil
	}
	if state.ErrorText != "" && state.ReservationStatus == "released" {
		if err := l.finishFailed(ctx, state, state.ActiveReservation); err != nil {
			return state, nil, false, err
		}
		return state, nil, true, storedTurnError(state)
	}
	if state.ActiveReservation != "" && state.ReservationStatus == "held" && !state.ReservationExpired {
		return state, nil, false, ErrTurnInFlight
	}
	return state, nil, false, nil
}

func storedTurnError(state TurnState) error {
	detail := strings.TrimSpace(state.ErrorText)
	if detail == "" {
		detail = "no stored failure detail"
	}
	return fmt.Errorf("%w: %s", ErrTurnFailed, detail)
}

// Begin creates the workspace conversation and turn for a new request id,
// serialized per (tenant, end user, release).
func (l *TurnLedger) Begin(ctx context.Context, tenantID int64, endUserRef, requestID, taskKind, inputSummary, inputDigest, status string, release domain.AgentReleaseReference) (TurnState, bool, error) {
	ctx = context.WithoutCancel(ctx)
	tx, err := l.DB.BeginTx(ctx, turnLedgerQueries)
	if err != nil {
		return TurnState{}, false, fmt.Errorf("begin turn: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	candidateID := fmt.Sprintf("%s-%d", l.Namespace, l.queries(ctx).GenID())
	workspaceLock := fmt.Sprintf("%s:%d:%s:%s:%s", l.Namespace, tenantID, endUserRef, release.AgentID, release.Version)
	if _, err = tx.Query(ctx, qLedgerLock, workspaceLock); err != nil {
		return TurnState{}, false, fmt.Errorf("lock workspace conversation: %w", err)
	}
	if _, err = tx.Query(ctx, qLedgerEnsureConversation,
		tenantID, candidateID, release.AgentID, release.Version, endUserRef,
		tenantID, endUserRef, release.AgentID, release.Version); err != nil {
		return TurnState{}, false, fmt.Errorf("ensure conversation: %w", err)
	}
	conversation, err := tx.Query(ctx, qLedgerGetConversation, tenantID, endUserRef, release.AgentID, release.Version)
	if err != nil {
		return TurnState{}, false, fmt.Errorf("load conversation: %w", err)
	}
	if len(conversation.Rows) == 0 {
		return TurnState{}, false, errors.New("load conversation: row missing after insert")
	}
	conversationID := keelcommon.AsString(conversation.Rows[0][0])
	if _, err = tx.Query(ctx, qLedgerLock, fmt.Sprintf("%d:%s", tenantID, conversationID)); err != nil {
		return TurnState{}, false, fmt.Errorf("lock conversation: %w", err)
	}
	inserted, err := tx.Query(ctx, qLedgerInsertTurn,
		tenantID, conversationID, requestID, status, l.inputURI(requestID), inputDigest, status,
		tenantID, conversationID)
	if err != nil {
		return TurnState{}, false, fmt.Errorf("insert turn: %w", err)
	}
	created := len(inserted.Rows) > 0
	if created {
		turnNo := keelcommon.AsInt64(inserted.Rows[0][0])
		if _, err = tx.Query(ctx, qLedgerInsertDetail,
			tenantID, conversationID, turnNo, taskKind,
			keelcommon.TruncateRunes(keelcommon.RedactForStorage(inputSummary), 400), release.Digest); err != nil {
			return TurnState{}, false, fmt.Errorf("insert turn detail: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return TurnState{}, false, fmt.Errorf("commit turn: %w", err)
	}
	committed = true
	state, found, err := l.findTurn(ctx, tenantID, requestID)
	if err != nil {
		return TurnState{}, false, err
	}
	if !found {
		return TurnState{}, false, fmt.Errorf("turn %q disappeared after creation", requestID)
	}
	return state, created, nil
}

// PrepareBudgeted resolves the request to a live execution claim: replay,
// create, quote, reserve, and CAS-activate; staged-but-unsettled results are
// settled and replayed instead of re-running.
func (l *TurnLedger) PrepareBudgeted(ctx context.Context, tenantID int64, endUserRef, requestID, taskKind, inputSummary, inputDigest, quotedInput string, agent contract.PricedAgent, release domain.AgentReleaseReference) (TurnExecution, any, bool, error) {
	state, stored, replay, err := l.Inspect(ctx, tenantID, requestID, taskKind, inputDigest)
	if err != nil || replay {
		return TurnExecution{}, stored, replay, err
	}
	found := state.RequestID != ""
	created := false
	if !found {
		state, created, err = l.Begin(ctx, tenantID, endUserRef, requestID, taskKind, inputSummary, inputDigest, "running", release)
		if err != nil {
			return TurnExecution{}, nil, false, err
		}
		if !created {
			state, stored, replay, err = l.Inspect(ctx, tenantID, requestID, taskKind, inputDigest)
			if err != nil || replay {
				return TurnExecution{}, stored, replay, err
			}
		}
	}
	quote, err := QuoteUsage(ctx, agent, quotedInput)
	if err != nil {
		if created {
			return TurnExecution{}, nil, false, l.FailUnreserved(ctx, state, err)
		}
		return TurnExecution{}, nil, false, err
	}
	if quote.Currency != l.Currency {
		err = fmt.Errorf("%w: quote currency %s, ledger requires %s", domain.ErrValidation, quote.Currency, l.Currency)
		if created {
			return TurnExecution{}, nil, false, l.FailUnreserved(ctx, state, err)
		}
		return TurnExecution{}, nil, false, err
	}
	reservation, err := l.Budget.Reserve(ctx, tenantID, requestID,
		quote.InputTokens+quote.OutputTokens, quote.CostMinorUnits, quote.Currency)
	if err != nil {
		if errors.Is(err, domain.ErrBudgetExceeded) && state.ResultPayload == "" {
			if failErr := l.FailUnreserved(ctx, state, err); failErr != nil && !errors.Is(failErr, ErrTurnFenced) {
				return TurnExecution{}, nil, false, errors.Join(err, failErr)
			}
		}
		return TurnExecution{}, nil, false, err
	}
	if err = l.activate(ctx, state, reservation); err != nil {
		return TurnExecution{}, nil, false, err
	}
	state.ActiveReservation = reservation.ReservationID
	state.ReservationAttempt = reservation.Attempt
	execution := TurnExecution{Turn: state, Reservation: reservation, Model: modelReferenceOf(agent)}
	if state.ResultPayload != "" {
		usage := domain.Usage{
			InputTokens: state.InputTokens, OutputTokens: state.OutputTokens,
			CostMinorUnits: state.CostMinorUnits, Currency: state.Currency,
		}
		if err = l.Budget.Commit(context.WithoutCancel(ctx), reservation, usage); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return TurnExecution{}, nil, false, ErrTurnFenced
			}
			return TurnExecution{}, nil, false, err
		}
		if err = l.finishSuccessful(ctx, state, reservation); err != nil {
			return TurnExecution{}, nil, false, err
		}
		return execution, json.RawMessage(state.ResultPayload), true, nil
	}
	return execution, nil, false, nil
}

func modelReferenceOf(agent contract.PricedAgent) domain.ModelReference {
	if carrier, ok := agent.(interface{ ModelReference() domain.ModelReference }); ok {
		return carrier.ModelReference()
	}
	return domain.ModelReference{}
}

func (l *TurnLedger) activate(ctx context.Context, state TurnState, reservation domain.BudgetReservation) error {
	result, err := l.queries(ctx).Query(context.WithoutCancel(ctx), qLedgerActivate,
		reservation.ReservationID, state.TenantID, state.ConversationID, state.TurnNo,
		keelcommon.NullIfEmpty(state.ActiveReservation), reservation.ReservationID, reservation.Attempt)
	if err != nil {
		return fmt.Errorf("activate reservation: %w", err)
	}
	if len(result.Rows) == 0 {
		return ErrTurnInFlight
	}
	return nil
}

// AttachJob links the turn to an external job inside the caller's transaction.
func (l *TurnLedger) AttachJob(ctx context.Context, qs keelport.QueryService, state TurnState, resultKind string, jobRef int64) error {
	attached, err := qs.Query(ctx, qLedgerAttachJob, resultKind, jobRef,
		state.TenantID, state.ConversationID, state.TurnNo)
	if err != nil {
		return fmt.Errorf("attach job: %w", err)
	}
	if len(attached.Rows) == 0 {
		return ErrTurnInFlight
	}
	return nil
}

func (l *TurnLedger) stageSuccessful(ctx context.Context, execution TurnExecution, resultKind string, payload any, usage domain.Usage, persist func(context.Context, keelport.QueryService) error) (TurnState, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return TurnState{}, fmt.Errorf("marshal turn result: %w", err)
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := l.DB.BeginTx(ctx, turnLedgerQueries)
	if err != nil {
		return TurnState{}, fmt.Errorf("begin result staging: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	staged, err := tx.Query(ctx, qLedgerStageSuccess,
		resultKind, string(raw), usage.InputTokens, usage.OutputTokens, usage.CostMinorUnits, usage.Currency,
		execution.Turn.TenantID, execution.Turn.ConversationID, execution.Turn.TurnNo,
		execution.Reservation.ReservationID)
	if err != nil {
		return TurnState{}, fmt.Errorf("stage result: %w", err)
	}
	if len(staged.Rows) == 0 {
		return TurnState{}, ErrTurnFenced
	}
	if persist != nil {
		if err = persist(ctx, tx); err != nil {
			return TurnState{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return TurnState{}, fmt.Errorf("commit result staging: %w", err)
	}
	committed = true
	state := execution.Turn
	state.ResultKind = resultKind
	state.ResultPayload = string(raw)
	state.InputTokens = usage.InputTokens
	state.OutputTokens = usage.OutputTokens
	state.CostMinorUnits = usage.CostMinorUnits
	state.Currency = usage.Currency
	state.ActiveReservation = execution.Reservation.ReservationID
	state.ReservationAttempt = execution.Reservation.Attempt
	return state, nil
}

func (l *TurnLedger) finishSuccessful(ctx context.Context, state TurnState, reservation domain.BudgetReservation) error {
	ctx = context.WithoutCancel(ctx)
	tx, err := l.DB.BeginTx(ctx, turnLedgerQueries)
	if err != nil {
		return fmt.Errorf("begin finalization: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	responseDigest := DigestStrings(state.ResultPayload)
	finished, err := tx.Query(ctx, qLedgerFinishSuccess,
		l.resultURI(state.RequestID), responseDigest,
		state.TenantID, state.ConversationID, state.TurnNo, reservation.ReservationID)
	if err != nil {
		return fmt.Errorf("finish turn: %w", err)
	}
	if len(finished.Rows) == 0 {
		current, found, findErr := l.findTurn(ctx, state.TenantID, state.RequestID)
		if findErr != nil {
			return findErr
		}
		if !found || current.Status != "completed" || current.ResponseDigest != responseDigest {
			return ErrTurnFenced
		}
		_ = tx.Rollback(ctx)
		committed = true
		return nil
	}
	usage := domain.Usage{
		InputTokens: state.InputTokens, OutputTokens: state.OutputTokens,
		CostMinorUnits: state.CostMinorUnits, Currency: state.Currency,
	}
	subject := state.AgentID + "@" + state.AgentVersion
	if err = l.InsertUsageEvent(ctx, tx, state.TenantID, state.ConversationID, state.TurnNo, subject, usage); err != nil {
		return err
	}
	if l.OnSettled != nil {
		if err = l.OnSettled(ctx, tx, state, usage); err != nil {
			return fmt.Errorf("settle hook: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit finalization: %w", err)
	}
	committed = true
	return nil
}

// Complete stages the result, settles the reservation, and finalizes the turn.
func (l *TurnLedger) Complete(ctx context.Context, execution TurnExecution, resultKind string, payload any, usage domain.Usage, persist func(context.Context, keelport.QueryService) error) error {
	state, err := l.stageSuccessful(ctx, execution, resultKind, payload, usage, persist)
	if err != nil {
		return err
	}
	if err = l.Budget.Commit(context.WithoutCancel(ctx), execution.Reservation, usage); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return ErrTurnFenced
		}
		return fmt.Errorf("settle budget: %w", err)
	}
	if err = l.finishSuccessful(ctx, state, execution.Reservation); err != nil {
		return err
	}
	if l.Activity != nil {
		release := domain.AgentReleaseReference{AgentID: state.AgentID, Version: state.AgentVersion, Digest: state.ReleaseDigest}
		if err := l.Activity.Record(context.WithoutCancel(ctx), state.TenantID, release, state.TaskKind); err != nil {
			// Attribution must never fail the task it describes.
			_ = err
		}
	}
	l.Metrics.RecordTurn(ctx, state.TaskKind, "completed", execution.Model, state.AgentVersion, turnLatency(state), usage)
	return nil
}

// Fail stages the failure, releases the reservation, and returns the cause.
func (l *TurnLedger) Fail(ctx context.Context, execution TurnExecution, cause error) error {
	errText := keelcommon.TruncateRunes(keelcommon.RedactForStorage(cause.Error()), 400)
	staged, err := l.queries(ctx).Query(context.WithoutCancel(ctx), qLedgerStageFailure,
		errText, execution.Turn.TenantID, execution.Turn.ConversationID,
		execution.Turn.TurnNo, execution.Reservation.ReservationID)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("stage failure: %w", err))
	}
	if len(staged.Rows) == 0 {
		return ErrTurnFenced
	}
	if err = l.Budget.Release(context.WithoutCancel(ctx), execution.Reservation); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return ErrTurnFenced
		}
		return errors.Join(cause, fmt.Errorf("release budget: %w", err))
	}
	state := execution.Turn
	state.ErrorText = errText
	if err = l.finishFailed(ctx, state, execution.Reservation.ReservationID); err != nil {
		return errors.Join(cause, err)
	}
	l.Metrics.RecordTurn(ctx, state.TaskKind, "failed", execution.Model, state.AgentVersion, turnLatency(state), domain.Usage{})
	return cause
}

func (l *TurnLedger) finishFailed(ctx context.Context, state TurnState, reservationID string) error {
	result, err := l.queries(ctx).Query(context.WithoutCancel(ctx), qLedgerFinishFailure,
		state.TenantID, state.ConversationID, state.TurnNo, reservationID)
	if err != nil {
		return fmt.Errorf("finish failed turn: %w", err)
	}
	if len(result.Rows) == 0 {
		current, found, findErr := l.findTurn(context.WithoutCancel(ctx), state.TenantID, state.RequestID)
		if findErr != nil {
			return findErr
		}
		if !found || current.Status != "failed" {
			return ErrTurnFenced
		}
	}
	return nil
}

// FailUnreserved fails a turn that never acquired a reservation.
func (l *TurnLedger) FailUnreserved(ctx context.Context, state TurnState, cause error) error {
	result, err := l.queries(ctx).Query(context.WithoutCancel(ctx), qLedgerFailUnreserved,
		state.TenantID, state.ConversationID, state.TurnNo,
		keelcommon.TruncateRunes(keelcommon.RedactForStorage(cause.Error()), 400))
	if err != nil {
		return errors.Join(cause, fmt.Errorf("fail unreserved turn: %w", err))
	}
	if len(result.Rows) == 0 {
		return ErrTurnFenced
	}
	return cause
}

// InsertUsageEvent writes the settled usage event, once per (turn, category).
func (l *TurnLedger) InsertUsageEvent(ctx context.Context, qs keelport.QueryService, tenantID int64, conversationID string, turnNo int64, subjectRef string, usage domain.Usage) error {
	_, err := qs.Query(ctx, qLedgerInsertUsageEvent,
		tenantID, conversationID, turnNo, l.UsageCategory, subjectRef,
		usage.InputTokens, usage.OutputTokens, usage.CostMinorUnits, usage.Currency,
		tenantID, conversationID, turnNo, l.UsageCategory)
	if err != nil {
		return fmt.Errorf("persist usage event: %w", err)
	}
	return nil
}

// Job-keyed transitions for workers executing queued turns; the caller's
// query map must include TurnLedgerQueries().

// SetJobTurnStatus moves a queued/running job turn to the given status.
func SetJobTurnStatus(ctx context.Context, qs keelport.QueryService, tenantID, jobRef int64, taskKind, status string) (bool, error) {
	result, err := qs.Query(ctx, qLedgerSetJobStatus, status, status, status, tenantID, jobRef, taskKind)
	if err != nil {
		return false, err
	}
	return len(result.Rows) > 0, nil
}

// ActivateJobReservation claims the job turn, replacing an expired attempt.
func ActivateJobReservation(ctx context.Context, qs keelport.QueryService, tenantID, jobRef int64, taskKind, priorReservationID string, reservation domain.BudgetReservation) (string, int64, bool, error) {
	result, err := qs.Query(ctx, qLedgerActivateJob,
		reservation.ReservationID, tenantID, jobRef, taskKind,
		keelcommon.NullIfEmpty(priorReservationID), reservation.ReservationID)
	if err != nil || len(result.Rows) == 0 {
		return "", 0, false, err
	}
	return keelcommon.AsString(result.Rows[0][0]), keelcommon.AsInt64(result.Rows[0][1]), true, nil
}

// StageJobUsage stages actual usage before settlement.
func StageJobUsage(ctx context.Context, qs keelport.QueryService, tenantID, jobRef int64, taskKind, reservationID string, usage domain.Usage) (bool, error) {
	result, err := qs.Query(ctx, qLedgerStageJobUsage,
		usage.InputTokens, usage.OutputTokens, usage.CostMinorUnits, usage.Currency,
		tenantID, jobRef, taskKind, reservationID)
	if err != nil {
		return false, err
	}
	return len(result.Rows) > 0, nil
}

// AttachJobArtifact links the produced artifact to the claimed job turn.
func AttachJobArtifact(ctx context.Context, qs keelport.QueryService, tenantID, jobRef, artifactRef int64, taskKind, reservationID string) (bool, error) {
	result, err := qs.Query(ctx, qLedgerAttachJobArtifact,
		artifactRef, tenantID, jobRef, taskKind, reservationID)
	if err != nil {
		return false, err
	}
	return len(result.Rows) > 0, nil
}

// CompleteJobTurn finalizes the job turn; fenced by the settled reservation.
func CompleteJobTurn(ctx context.Context, qs keelport.QueryService, tenantID, jobRef int64, taskKind, reservationID, responseURI, responseDigest string) (bool, error) {
	result, err := qs.Query(ctx, qLedgerCompleteJobTurn,
		responseURI, responseDigest, tenantID, jobRef, taskKind, reservationID)
	if err != nil {
		return false, err
	}
	return len(result.Rows) > 0, nil
}

func turnLatency(state TurnState) time.Duration {
	if state.QueuedAt.IsZero() {
		return 0
	}
	return time.Since(state.QueuedAt)
}

func optionalInt64(value any) int64 {
	if number, ok := keelcommon.AsInt64OK(value); ok {
		return number
	}
	return 0
}
