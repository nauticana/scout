package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelmodel "github.com/nauticana/keel/model"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/observability"
)

const (
	qRecordLock         = "scout_turn_record_lock"
	qRecordFind         = "scout_turn_record_find"
	qRecordOpen         = "scout_turn_record_open"
	qRecordAcquire      = "scout_turn_record_acquire"
	qRecordAcquireProbe = "scout_turn_record_acquire_probe"
	qRecordConversation = "scout_turn_record_ensure_conversation"
	qRecordPinnedAgent  = "scout_turn_record_pinned_agent"
	qRecordDetail       = "scout_turn_record_detail"
	qRecordFail         = "scout_turn_record_fail"
	qRecordFailParked   = "scout_turn_record_fail_parked"
	qRecordHold         = "scout_turn_record_hold"
	qRecordSuspend      = "scout_turn_record_suspend"
	qRecordResume       = "scout_turn_record_resume"
	qRecordOwnerGuard   = "scout_turn_record_owner_guard"
	qRecordOwnerTurn    = "scout_turn_record_owner_turn"
	qRecordOwnerState   = "scout_turn_record_owner_state"
	qRecordAddress      = "scout_turn_record_address"
	qRecordAdmits       = "scout_turn_record_admits"
	qRecordRelease      = "scout_turn_record_release"
	qRecordSelectable   = "scout_turn_record_selectable"
)

// currentLease matches the claim's queue row only while that claim is its live lease, by the store clock.
const currentLease = `
SELECT 1 FROM turn_queue q
 WHERE q.id = ? AND q.partner_id = turn.partner_id AND q.request_id = turn.request_id
   AND q.lease_token = ? AND q.worker_id = ? AND q.status_code = ` + sqlQueueLeased + `
   AND q.lease_until > CURRENT_TIMESTAMP`

// failedTurn mirrors a failure onto the turn's history row in the statement that records it.
const failedTurn = `
mirrored AS (
UPDATE conversation_turn_detail detail
   SET error_text = ?
  FROM failed
 WHERE detail.partner_id = failed.partner_id AND detail.conversation_id = failed.conversation_id AND detail.turn_no = failed.turn_no
RETURNING detail.turn_no)
SELECT turn_no FROM failed`

var turnRecordQueries = map[string]string{
	qRecordLock: `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`,

	qRecordFind: `
SELECT turn_no, conversation_id, status_code, input_digest, response_uri, response_digest, fence
  FROM conversation_turn
 WHERE partner_id = ? AND request_id = ?`,

	qRecordOwnerGuard: turnOwnerGuard,
	// The guarded touch of an ending turn, naming the conversation its usage events belong to.
	qRecordOwnerTurn: `
UPDATE conversation_turn SET fence = fence
 WHERE partner_id = ? AND request_id = ? AND fence = ? AND status_code IN (` + sqlTurnOwned + `)
RETURNING turn_no, conversation_id`,
	qRecordOwnerState: `
SELECT fence, status_code, cancel_requested_at IS NOT NULL, COALESCE(cancel_reason, '')
  FROM conversation_turn
 WHERE partner_id = ? AND request_id = ?`,

	// The named agent, else the conversation's agent, else the alias target, when it is of
	// the alias's type; moving the type's default does not move a live conversation.
	qRecordAddress: `
SELECT p.agent_id
  FROM agent_profile p
  LEFT JOIN agent_alias a ON a.partner_id = p.partner_id AND a.alias_id = ?
 WHERE p.partner_id = ?
   AND p.agent_id = COALESCE(NULLIF(?, ''),
       (SELECT c.agent_id FROM agent_conversation c WHERE c.partner_id = p.partner_id AND c.conversation_id = ?), a.agent_id)
   AND (? = '' OR a.agent_type_id = p.agent_type_id)`,
	// Admission closes when the agent is disabled, suspended, draining, archived, or retired.
	qRecordAdmits: `
SELECT s.admits_work AND d.enabled
  FROM agent_profile p
  JOIN agent_state s ON s.code = p.state_code
  JOIN agent_draft d ON d.partner_id = p.partner_id AND d.agent_id = p.agent_id
 WHERE p.partner_id = ? AND p.agent_id = ?
   FOR SHARE OF p`,
	qRecordSelectable: `
SELECT p.agent_id, p.display_name, p.agent_id = a.agent_id, dep.stable_version
  FROM agent_alias a
  JOIN agent_profile p ON p.partner_id = a.partner_id AND p.agent_type_id = a.agent_type_id
  JOIN agent_state s ON s.code = p.state_code
  JOIN agent_draft d ON d.partner_id = p.partner_id AND d.agent_id = p.agent_id
  JOIN agent_deployment dep ON dep.partner_id = p.partner_id AND dep.agent_id = p.agent_id
 WHERE a.partner_id = ? AND a.alias_id = ? AND s.admits_work AND d.enabled
 ORDER BY p.agent_id = a.agent_id DESC, p.display_name, p.agent_id`,
	qRecordRelease: `
SELECT 1 FROM agent_version v
 WHERE v.partner_id = ? AND v.agent_id = ? AND v.agent_version = ?
   AND NOT EXISTS (SELECT 1 FROM agent_version_quarantine q
                    WHERE q.partner_id = v.partner_id AND q.agent_id = v.agent_id
                      AND q.agent_version = v.agent_version AND q.lifted_at IS NULL)`,
	// A new conversation pins the chosen release, else the agent's deployed stable version;
	// an existing one keeps its pin.
	qRecordConversation: `
INSERT INTO agent_conversation (partner_id, conversation_id, agent_id, agent_version, version_source, end_user_ref, alias_id, pin_reason)
SELECT dep.partner_id, ?, dep.agent_id, COALESCE(NULLIF(?, ''), dep.stable_version), ?, ?, NULLIF(?, ''), NULLIF(?, '')
  FROM agent_deployment dep
 WHERE dep.partner_id = ? AND dep.agent_id = ?
ON CONFLICT (partner_id, conversation_id) DO NOTHING`,
	qRecordPinnedAgent: `
SELECT agent_id, agent_version
  FROM agent_conversation
 WHERE partner_id = ? AND conversation_id = ?`,

	// The same history row TurnLedger writes, so turn listings include runtime turns.
	qRecordDetail: `
INSERT INTO conversation_turn_detail (partner_id, conversation_id, turn_no, task_kind, input_summary, release_digest)
SELECT c.partner_id, c.conversation_id, ?, ?, ?, v.definition_digest
  FROM agent_conversation c
  JOIN agent_version v ON v.partner_id = c.partner_id AND v.agent_id = c.agent_id AND v.agent_version = c.agent_version
 WHERE c.partner_id = ? AND c.conversation_id = ?
ON CONFLICT (partner_id, conversation_id, turn_no) DO NOTHING`,

	qRecordOpen: `
INSERT INTO conversation_turn
       (partner_id, conversation_id, turn_no, request_id, status_code, input_uri, input_digest)
SELECT ?, ?, COALESCE(MAX(turn_no), 0) + 1, ?, ` + sqlTurnQueued + `, ?, ?
  FROM conversation_turn
 WHERE partner_id = ? AND conversation_id = ?
ON CONFLICT DO NOTHING
RETURNING turn_no`,

	// The claim is checked in the statement that moves the fence: a delayed or forged delivery
	// matches no current lease, so it can neither acquire the turn nor evict its owner.
	qRecordAcquire: `
UPDATE conversation_turn turn
   SET status_code = ` + sqlTurnRunning + `, fence = turn.fence + 1, started_at = COALESCE(turn.started_at, CURRENT_TIMESTAMP)
 WHERE turn.partner_id = ? AND turn.request_id = ?
   AND turn.status_code IN (` + sqlTurnQueued + `, ` + sqlTurnOwned + `)
   AND EXISTS (` + currentLease + `)
RETURNING turn.turn_no, turn.fence`,
	qRecordAcquireProbe: `
SELECT turn.status_code, EXISTS (` + currentLease + `)
  FROM conversation_turn turn
 WHERE turn.partner_id = ? AND turn.request_id = ?`,

	// The worker holds its turn at a checkpoint or on an unknown call outcome, and a pending hold
	// request is cleared as the hold is taken. A hold or suspension yields to a cancellation
	// requested before it, which nothing could wake out of a parked turn; the worker ends it instead.
	qRecordHold: `
UPDATE conversation_turn
   SET status_code = ` + sqlTurnHeld + `, fence = fence + 1, hold_code = ?,
       held_at = CURRENT_TIMESTAMP, hold_requested_at = NULL, hold_reason = NULL
 WHERE partner_id = ? AND request_id = ? AND fence = ? AND status_code IN (` + sqlTurnOwned + `)
   AND cancel_requested_at IS NULL
RETURNING fence`,

	// Suspend and Resume move only between live states, so a terminal turn is
	// never revived and a settled reservation is never re-held.
	qRecordSuspend: `
UPDATE conversation_turn
   SET status_code = ` + sqlTurnSuspended + `, fence = fence + 1
 WHERE partner_id = ? AND request_id = ? AND fence = ? AND status_code IN (` + sqlTurnOwned + `)
   AND cancel_requested_at IS NULL
RETURNING fence`,

	qRecordResume: `
UPDATE conversation_turn
   SET status_code = ` + sqlTurnQueued + `, fence = fence + 1
 WHERE partner_id = ? AND request_id = ? AND status_code = ` + sqlTurnSuspended + `
RETURNING turn_no`,

	// The history row, when the turn has one, carries the same error in the same statement.
	qRecordFail: `
WITH failed AS (
UPDATE conversation_turn
   SET status_code = ?, response_uri = ?, response_digest = ?,
       started_at = COALESCE(started_at, CURRENT_TIMESTAMP), completed_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND request_id = ? AND fence = ? AND status_code IN (` + sqlTurnOwned + `)
RETURNING partner_id, conversation_id, turn_no),` + failedTurn,

	qRecordFailParked: `
WITH failed AS (
UPDATE conversation_turn
   SET status_code = ` + sqlTurnFailed + `, response_uri = ?, response_digest = ?,
       started_at = COALESCE(started_at, CURRENT_TIMESTAMP), completed_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND request_id = ? AND status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `)
RETURNING partner_id, conversation_id, turn_no),` + failedTurn,
}

// TableTurnRecordStore is the TurnRecordStore over conversation_turn: the
// request-keyed turn identity and its lifecycle status. A failed turn's settlement and usage
// events are written in the transaction that fails it. Failure payloads (the error code) go
// through the object codec like responses do.
type TableTurnRecordStore struct {
	DB      port.DatabaseRepository
	Objects ObjectStateCodec
	// Settlement writes a failing turn's budget and usage in its failure's transaction.
	Settlement TurnSettlementWriter
	// TaskKind labels runtime turns in conversation_turn_detail; empty skips the history row.
	TaskKind string
	// Versions chooses a new conversation's release by the agent's traffic policy; nil takes
	// the stable release.
	Versions contract.AgentVersionResolutionExplainer

	once sync.Once
	qs   port.QueryService
}

var _ contract.TurnRecordStore = (*TableTurnRecordStore)(nil)

func (store *TableTurnRecordStore) validate() error {
	if store.DB == nil || store.Objects == nil {
		return fmt.Errorf("%w: turn record store needs a database and an object codec", domain.ErrValidation)
	}
	return nil
}

func (store *TableTurnRecordStore) queryMap() map[string]string {
	return common.MergeMaps(TurnLedgerQueries(), turnRecordQueries, turnRateQueries, turnSettlementQueries)
}

func (store *TableTurnRecordStore) queries(ctx context.Context) port.QueryService {
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, store.queryMap()) })
	return store.qs
}

type turnRecordRow struct {
	turnNo         int64
	conversationID string
	status         domain.TurnStatus
	inputDigest    string
	response       domain.ObjectRef
	fence          int64
}

func (store *TableTurnRecordStore) find(ctx context.Context, tenantID int64, requestID string) (turnRecordRow, bool, error) {
	result, err := store.queries(ctx).Query(ctx, qRecordFind, tenantID, requestID)
	if err != nil {
		return turnRecordRow{}, false, fmt.Errorf("find turn record: %w", err)
	}
	if len(result.Rows) == 0 {
		return turnRecordRow{}, false, nil
	}
	row := result.Rows[0]
	if len(row) < 7 {
		return turnRecordRow{}, false, fmt.Errorf("decode turn record: expected 7 columns, got %d", len(row))
	}
	return turnRecordRow{
		turnNo: common.AsInt64(row[0]), conversationID: common.AsString(row[1]),
		status: domain.TurnStatus(common.AsString(row[2])), inputDigest: common.AsString(row[3]),
		response: domain.ObjectRef{URI: common.AsString(row[4]), Digest: common.AsString(row[5])},
		fence:    common.AsInt64(row[6]),
	}, true, nil
}

// Address resolves the agent that serves a turn: agentID when it is of the alias's type,
// else the agent the conversation runs on, else the alias target. An unknown alias or an
// agent of another type is ErrNotFound.
func (store *TableTurnRecordStore) Address(ctx context.Context, tenantID int64, aliasID, agentID, conversationID string) (string, error) {
	if tenantID <= 0 || strings.TrimSpace(aliasID) == "" && strings.TrimSpace(agentID) == "" {
		return "", fmt.Errorf("%w: tenant and an agent type or agent are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return "", err
	}
	result, err := store.queries(ctx).Query(ctx, qRecordAddress, aliasID, tenantID, agentID, conversationID, aliasID)
	if err != nil {
		return "", fmt.Errorf("address turn agent: %w", err)
	}
	if len(result.Rows) == 0 {
		return "", fmt.Errorf("%w: no agent %q of type %q", domain.ErrNotFound, agentID, aliasID)
	}
	return common.AsString(result.Rows[0][0]), nil
}

// Selectable lists the deployed agents of aliasID's type that admit work, the alias target first.
func (store *TableTurnRecordStore) Selectable(ctx context.Context, tenantID int64, aliasID string) ([]domain.SelectableAgent, error) {
	if tenantID <= 0 || strings.TrimSpace(aliasID) == "" {
		return nil, fmt.Errorf("%w: tenant and agent type are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return nil, err
	}
	result, err := store.queries(ctx).Query(ctx, qRecordSelectable, tenantID, aliasID)
	if err != nil {
		return nil, fmt.Errorf("list selectable agents: %w", err)
	}
	agents := make([]domain.SelectableAgent, 0, len(result.Rows))
	for _, row := range result.Rows {
		agents = append(agents, domain.SelectableAgent{
			AgentID: common.AsString(row[0]), DisplayName: common.AsString(row[1]),
			Default: common.AsBool(row[2]), Release: common.AsString(row[3]),
		})
	}
	return agents, nil
}

// pin starts a conversation on the release the caller pinned, else the one the traffic
// policy chooses, and returns the stored pin.
func (store *TableTurnRecordStore) pin(ctx context.Context, tx port.TxQueryService, request domain.TurnRequest) (*keelmodel.QueryResult, error) {
	tenantID := request.TenantContext.TenantID
	resolution := domain.AgentVersionResolution{Version: request.Release, Source: domain.VersionFromRequest}
	if request.Release == "" {
		resolution = domain.AgentVersionResolution{Source: domain.VersionFromStable}
		if store.Versions != nil {
			chosen, err := store.Versions.ExplainVersion(ctx, domain.VersionRequest{
				TenantID: tenantID, AgentID: request.AgentID, ConversationID: request.ConversationID, Subject: request.OnBehalfOf,
			})
			if err != nil {
				return nil, fmt.Errorf("open turn: choose release: %w", err)
			}
			resolution = chosen
		}
	}
	if _, err := tx.Query(ctx, qRecordConversation, request.ConversationID, resolution.Version, string(resolution.Source),
		endUserRef(request), request.AliasID, request.ReleaseReason, tenantID, request.AgentID); err != nil {
		return nil, fmt.Errorf("open turn: pin conversation: %w", err)
	}
	pinned, err := tx.Query(ctx, qRecordPinnedAgent, tenantID, request.ConversationID)
	if err != nil {
		return nil, fmt.Errorf("open turn: read conversation pin: %w", err)
	}
	return pinned, nil
}

// admits refuses a turn for an agent that no longer takes work, and a release pin to a
// version that does not exist or is quarantined.
func admits(ctx context.Context, tx port.TxQueryService, request domain.TurnRequest) error {
	tenantID := request.TenantContext.TenantID
	state, err := tx.Query(ctx, qRecordAdmits, tenantID, request.AgentID)
	if err != nil {
		return fmt.Errorf("open turn: read agent state: %w", err)
	}
	if len(state.Rows) == 0 {
		return fmt.Errorf("%w: agent %q", domain.ErrNotFound, request.AgentID)
	}
	if !common.AsBool(state.Rows[0][0]) {
		return fmt.Errorf("%w: agent %q is not accepting work", domain.ErrConflict, request.AgentID)
	}
	if request.Release == "" {
		return nil
	}
	release, err := tx.Query(ctx, qRecordRelease, tenantID, request.AgentID, request.Release)
	if err != nil {
		return fmt.Errorf("open turn: read pinned release: %w", err)
	}
	if len(release.Rows) == 0 {
		return fmt.Errorf("%w: release %q of agent %q is unknown or quarantined", domain.ErrNotFound, request.Release, request.AgentID)
	}
	return nil
}

// Open inserts the turn under a per-conversation advisory lock so turn numbers
// stay dense; a replay of the same request id returns the existing turn. An agent's
// turn reads its conversation's frozen release in the same transaction and is refused
// when the release is missing or its turns per minute are spent.
func (store *TableTurnRecordStore) Open(ctx context.Context, request domain.TurnRequest, input domain.ObjectRef) (int64, error) {
	tenantID := request.TenantContext.TenantID
	if tenantID <= 0 || strings.TrimSpace(request.RequestID) == "" || len([]rune(request.RequestID)) > 120 ||
		strings.TrimSpace(request.ConversationID) == "" || input.URI == "" || len(input.Digest) != 64 {
		return 0, fmt.Errorf("%w: tenant, request, conversation, and input reference are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return 0, err
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := store.DB.BeginTx(ctx, store.queryMap())
	if err != nil {
		return 0, fmt.Errorf("open turn: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Query(ctx, qRecordLock, fmt.Sprintf("%d:%s", tenantID, request.ConversationID)); err != nil {
		return 0, fmt.Errorf("open turn: lock conversation: %w", err)
	}
	if strings.TrimSpace(request.AgentID) != "" {
		if err = admits(ctx, tx, request); err != nil {
			return 0, err
		}
		pinned, pinErr := tx.Query(ctx, qRecordPinnedAgent, tenantID, request.ConversationID)
		if pinErr != nil {
			return 0, fmt.Errorf("open turn: read conversation pin: %w", pinErr)
		}
		if len(pinned.Rows) == 0 {
			if pinned, err = store.pin(ctx, tx, request); err != nil {
				return 0, err
			}
		}
		if len(pinned.Rows) == 0 {
			return 0, fmt.Errorf("%w: agent %q has no deployed version to pin", domain.ErrNotReady, request.AgentID)
		}
		if pinnedAgent := common.AsString(pinned.Rows[0][0]); pinnedAgent != request.AgentID {
			return 0, fmt.Errorf("%w: conversation %q is pinned to agent %q, not %q", domain.ErrConflict, request.ConversationID, pinnedAgent, request.AgentID)
		}
		pinnedVersion := strings.TrimSpace(common.AsString(pinned.Rows[0][1]))
		if pinnedVersion == "" {
			return 0, fmt.Errorf("%w: conversation %q has no pinned agent version", domain.ErrNotReady, request.ConversationID)
		}
		if request.Release != "" && pinnedVersion != request.Release {
			return 0, fmt.Errorf("%w: conversation %q is pinned to release %q, not %q", domain.ErrConflict, request.ConversationID, pinnedVersion, request.Release)
		}
		if err = admitRate(ctx, tx, turnRateRequest{
			TenantID: tenantID, ScopeID: request.TenantContext.ScopeID, AgentID: request.AgentID,
			Release: pinnedVersion, RequestID: request.RequestID,
		}); err != nil {
			return 0, err
		}
	}
	inserted, err := tx.Query(ctx, qRecordOpen,
		tenantID, request.ConversationID, request.RequestID, input.URI, input.Digest,
		tenantID, request.ConversationID)
	if err != nil {
		return 0, fmt.Errorf("open turn: insert: %w", err)
	}
	if len(inserted.Rows) > 0 && store.TaskKind != "" && strings.TrimSpace(request.AgentID) != "" {
		summary := common.TruncateRunes(common.RedactForStorage(string(request.Input)), 400)
		if _, err = tx.Query(ctx, qRecordDetail, common.AsInt64(inserted.Rows[0][0]), store.TaskKind, summary, tenantID, request.ConversationID); err != nil {
			return 0, fmt.Errorf("open turn: history row: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("open turn: commit: %w", err)
	}
	committed = true
	if len(inserted.Rows) > 0 {
		return common.AsInt64(inserted.Rows[0][0]), nil
	}
	existing, found, err := store.find(ctx, tenantID, request.RequestID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("open turn: request %q disappeared after insert", request.RequestID)
	}
	if existing.inputDigest != input.Digest || existing.conversationID != request.ConversationID {
		return 0, fmt.Errorf("%w: request %q was already used with different input", domain.ErrConflict, request.RequestID)
	}
	return existing.turnNo, nil
}

// Find resolves the request; terminal payloads are hydrated and digest-verified.
func (store *TableTurnRecordStore) Find(ctx context.Context, tenantID int64, requestID string) (domain.TurnRecord, error) {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return domain.TurnRecord{}, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return domain.TurnRecord{}, err
	}
	row, found, err := store.find(ctx, tenantID, requestID)
	if err != nil {
		return domain.TurnRecord{}, err
	}
	if !found {
		return domain.TurnRecord{}, fmt.Errorf("%w: turn request %q", domain.ErrNotFound, requestID)
	}
	record := domain.TurnRecord{TurnNo: row.turnNo, Status: row.status, Fence: row.fence}
	if row.response.URI == "" {
		return record, nil
	}
	if record.Payload, err = store.Objects.Hydrate(ctx, row.response); err != nil {
		return domain.TurnRecord{}, fmt.Errorf("hydrate turn %q payload: %w", requestID, err)
	}
	return record, nil
}

// Acquire takes the turn for the claim's delivery with its fence incremented; see the contract
// for how a refused acquisition reads.
func (store *TableTurnRecordStore) Acquire(ctx context.Context, claim domain.TurnClaim) (domain.TurnOwner, error) {
	turn := claim.Dispatch.Turn
	tenantID, requestID := turn.TenantContext.TenantID, turn.RequestID
	if err := claim.Validate(); err != nil {
		return domain.TurnOwner{}, err
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return domain.TurnOwner{}, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return domain.TurnOwner{}, err
	}
	ctx = context.WithoutCancel(ctx)
	acquired, err := store.queries(ctx).Query(ctx, qRecordAcquire, tenantID, requestID, claim.QueueID, claim.LeaseToken, claim.WorkerID)
	if err != nil {
		return domain.TurnOwner{}, fmt.Errorf("acquire turn: %w", err)
	}
	if len(acquired.Rows) > 0 {
		return domain.TurnOwner{TenantID: tenantID, RequestID: requestID, Fence: common.AsInt64(acquired.Rows[0][1])}, nil
	}
	probe, err := store.queries(ctx).Query(ctx, qRecordAcquireProbe, claim.QueueID, claim.LeaseToken, claim.WorkerID, tenantID, requestID)
	if err != nil {
		return domain.TurnOwner{}, fmt.Errorf("acquire turn: read refusal: %w", err)
	}
	if len(probe.Rows) == 0 {
		return domain.TurnOwner{}, fmt.Errorf("%w: turn request %q", domain.ErrNotFound, requestID)
	}
	return domain.TurnOwner{}, refusedAcquisition(requestID, domain.TurnStatus(common.AsString(probe.Rows[0][0])), common.AsBool(probe.Rows[0][1]))
}

// refusedAcquisition explains why a claim did not acquire its turn.
func refusedAcquisition(requestID string, status domain.TurnStatus, currentLease bool) error {
	switch {
	case !currentLease:
		return fmt.Errorf("%w: delivery of turn %q is not the current lease of its queue row", domain.ErrFenceLost, requestID)
	case status == domain.TurnHeld || status == domain.TurnOperated || status == domain.TurnSuspended:
		return fmt.Errorf("%w: turn %q is %s", domain.ErrTurnHeld, requestID, status)
	}
	return fmt.Errorf("%w: turn %q is %s", domain.ErrConflict, requestID, status)
}

// Hold takes the owner's running turn to held under code, a checkpoint or an unknown call
// outcome, and returns the held fence; a cancellation requested before it is
// domain.ErrTurnCanceled, for the owner to end the turn.
func (store *TableTurnRecordStore) Hold(ctx context.Context, owner domain.TurnOwner, code domain.TurnHoldCode) (int64, error) {
	if code != domain.HoldCheckpoint && code != domain.HoldUnknownOutcome {
		return 0, fmt.Errorf("%w: a worker holds its turn at a checkpoint or on an unknown outcome", domain.ErrValidation)
	}
	return store.ownerTransition(ctx, owner, qRecordHold, "hold turn", string(code))
}

// Fail stores the error code as the owner's turn's terminal payload, in one transaction with the
// settlement: guarded touch, budget, usage events, failure. A reservation no longer held does
// not keep the turn from failing; the turn fails and domain.ErrBudgetUnsettled says so.
func (store *TableTurnRecordStore) Fail(ctx context.Context, owner domain.TurnOwner, status domain.TurnStatus, errorCode string, settlement domain.TurnSettlement) error {
	if err := owner.Check(); err != nil {
		return err
	}
	if (status != domain.TurnFailed && status != domain.TurnCancelled) || strings.TrimSpace(errorCode) == "" {
		return fmt.Errorf("%w: a failed or cancelled status and an error code are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return err
	}
	if err := store.Settlement.validate(); err != nil {
		return err
	}
	ref, err := store.Objects.Dehydrate(ctx, turnFailureName(owner.TenantID, owner.RequestID, errorCode), []byte(errorCode))
	if err != nil {
		return fmt.Errorf("dehydrate turn failure: %w", err)
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := store.DB.BeginTx(ctx, store.queryMap())
	if err != nil {
		return fmt.Errorf("fail turn: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	touched, err := tx.Query(ctx, qRecordOwnerTurn, owner.TenantID, owner.RequestID, owner.Fence)
	if err != nil {
		return fmt.Errorf("fail turn: guard turn owner: %w", err)
	}
	if len(touched.Rows) == 0 || len(touched.Rows[0]) < 2 {
		return fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence)
	}
	turnNo, conversationID := common.AsInt64(touched.Rows[0][0]), common.AsString(touched.Rows[0][1])
	unheld, err := store.Settlement.write(ctx, tx, owner.TenantID, owner.RequestID, conversationID, turnNo, settlement)
	if err != nil {
		return err
	}
	failed, err := tx.Query(ctx, qRecordFail, string(status), ref.URI, ref.Digest, owner.TenantID, owner.RequestID, owner.Fence, errorCode)
	if err != nil {
		return fmt.Errorf("fail turn: %w", err)
	}
	if len(failed.Rows) == 0 {
		return fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("fail turn: commit: %w", err)
	}
	committed = true
	if unheld != nil {
		return fmt.Errorf("%w: %w", domain.ErrBudgetUnsettled, unheld)
	}
	return nil
}

// FailParked fails a queued or suspended turn no worker runs.
func (store *TableTurnRecordStore) FailParked(ctx context.Context, tenantID int64, requestID, errorCode string) error {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" || strings.TrimSpace(errorCode) == "" {
		return fmt.Errorf("%w: tenant, request, and error code are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return err
	}
	ref, err := store.Objects.Dehydrate(ctx, turnFailureName(tenantID, requestID, errorCode), []byte(errorCode))
	if err != nil {
		return fmt.Errorf("dehydrate turn failure: %w", err)
	}
	ctx = context.WithoutCancel(ctx)
	failed, err := store.queries(ctx).Query(ctx, qRecordFailParked, ref.URI, ref.Digest, tenantID, requestID, errorCode)
	if err != nil {
		return fmt.Errorf("fail turn: %w", err)
	}
	if len(failed.Rows) > 0 {
		return nil
	}
	return store.refusedParked(ctx, tenantID, requestID, "")
}

// refusedParked reads why a transition of a turn no worker runs did not apply; a turn already
// in done, the status the transition leads to, is a repeat and succeeds.
func (store *TableTurnRecordStore) refusedParked(ctx context.Context, tenantID int64, requestID string, done domain.TurnStatus) error {
	row, found, err := store.find(ctx, tenantID, requestID)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w: turn request %q", domain.ErrNotFound, requestID)
	case row.status == domain.TurnHeld || row.status == domain.TurnOperated:
		return fmt.Errorf("%w: turn %q is %s", domain.ErrTurnHeld, requestID, row.status)
	case done != "" && row.status == done:
		return nil
	}
	return fmt.Errorf("%w: turn %q is %s", domain.ErrConflict, requestID, row.status)
}

// Suspend parks the owner's turn awaiting a human decision; a cancellation requested before it
// is domain.ErrTurnCanceled. The budget reservation is deliberately left held: settling it here
// would let the turn resume with no budget, and releasing it would let a tenant park work to
// dodge its ceiling.
func (store *TableTurnRecordStore) Suspend(ctx context.Context, owner domain.TurnOwner, reason string) (int64, error) {
	if strings.TrimSpace(reason) == "" {
		return 0, fmt.Errorf("%w: a suspension reason is required", domain.ErrValidation)
	}
	return store.ownerTransition(ctx, owner, qRecordSuspend, "suspend turn")
}

// ownerTransition applies one fenced transition of the owner's running turn and returns the new
// fence; leading are the statement's arguments before the owner's.
func (store *TableTurnRecordStore) ownerTransition(ctx context.Context, owner domain.TurnOwner, query, operation string, leading ...any) (int64, error) {
	if err := owner.Check(); err != nil {
		return 0, err
	}
	if err := store.validate(); err != nil {
		return 0, err
	}
	ctx = context.WithoutCancel(ctx)
	moved, err := store.queries(ctx).Query(ctx, query, append(leading, owner.TenantID, owner.RequestID, owner.Fence)...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", operation, err)
	}
	if len(moved.Rows) == 0 {
		return 0, store.refusedOwnerTransition(ctx, owner)
	}
	return common.AsInt64(moved.Rows[0][0]), nil
}

// refusedOwnerTransition tells a pending cancellation of the owner's own turn apart from a
// fence that moved on.
func (store *TableTurnRecordStore) refusedOwnerTransition(ctx context.Context, owner domain.TurnOwner) error {
	state, err := store.queries(ctx).Query(ctx, qRecordOwnerState, owner.TenantID, owner.RequestID)
	if err != nil {
		return fmt.Errorf("read turn state: %w", err)
	}
	if len(state.Rows) > 0 && len(state.Rows[0]) >= 4 && common.AsInt64(state.Rows[0][0]) == owner.Fence && common.AsBool(state.Rows[0][2]) {
		if status := domain.TurnStatus(common.AsString(state.Rows[0][1])); status == domain.TurnRunning || status == domain.TurnStreaming {
			return fmt.Errorf("%w: %s", domain.ErrTurnCanceled, common.AsString(state.Rows[0][3]))
		}
	}
	return fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence)
}

// Resume returns a suspended turn to the queue so a worker replays it from its
// last checkpoint; committed steps are not re-executed.
func (store *TableTurnRecordStore) Resume(ctx context.Context, tenantID int64, requestID string) error {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	resumed, err := store.queries(ctx).Query(ctx, qRecordResume, tenantID, requestID)
	if err != nil {
		return fmt.Errorf("resume turn: %w", err)
	}
	if len(resumed.Rows) > 0 {
		return nil
	}
	return store.refusedParked(ctx, tenantID, requestID, domain.TurnQueued)
}

func turnInputName(tenantID int64, requestID string) string {
	return fmt.Sprintf("turn-input/%d/%s", tenantID, requestID)
}

// turnFailureName gives each failure code its own object, so a late Fail of a terminal
// turn cannot overwrite the payload the turn's stored digest names.
func turnFailureName(tenantID int64, requestID, errorCode string) string {
	return fmt.Sprintf("turn-failure/%d/%s/%s", tenantID, requestID, DigestBytes([]byte(errorCode))[:12])
}

// terminalTurnFallbackCode is the client error code of a failed or cancelled turn that stored none.
func terminalTurnFallbackCode(status domain.TurnStatus) string {
	if status == domain.TurnCancelled {
		return "canceled"
	}
	return "failed"
}

// turnAuditCategory is the audit category that records a turn reaching status.
func turnAuditCategory(status domain.TurnStatus) string {
	switch status {
	case domain.TurnCompleted:
		return observability.AuditCategoryTurnCompleted
	case domain.TurnCancelled:
		return observability.AuditCategoryTurnCancelled
	case domain.TurnSuspended:
		return observability.AuditCategoryTurnSuspended
	}
	return observability.AuditCategoryTurnFailed
}

// endUserRef names the human a turn acts for, when there is one.
func endUserRef(request domain.TurnRequest) any {
	if request.OnBehalfOf.Kind != domain.PrincipalHuman || request.OnBehalfOf.ID == "" {
		return nil
	}
	return request.OnBehalfOf.ID
}
