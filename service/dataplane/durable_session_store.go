package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelmodel "github.com/nauticana/keel/model"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qSessionLoad             = "scout_session_load"
	qSessionInsertCheckpoint = "scout_session_insert_checkpoint"
	qSessionCreateSnapshot   = "scout_session_create_snapshot"
	qSessionAdvanceSnapshot  = "scout_session_advance_snapshot"
	qSessionActiveTurn       = "scout_session_active_turn"
	qSessionCompleteTurn     = "scout_session_complete_turn"
	qSessionCheckpointDigest = "scout_session_checkpoint_digest"
	qSessionResponseDigest   = "scout_session_response_digest"
	qSessionOwnerGuard       = "scout_session_owner_guard"
	qSessionTurnFence        = "scout_session_turn_fence"
)

var durableSessionQueries = map[string]string{
	qSessionLoad: `
SELECT conversation.agent_version, snapshot.latest_turn_no, snapshot.latest_step_no,
       snapshot.state_uri, snapshot.state_digest, snapshot.revision, step.step_id,
       conversation.end_user_ref
  FROM agent_conversation conversation
  LEFT JOIN session_snapshot snapshot
    ON snapshot.partner_id = conversation.partner_id
   AND snapshot.conversation_id = conversation.conversation_id
  LEFT JOIN step_checkpoint checkpoint
    ON checkpoint.partner_id = snapshot.partner_id
   AND checkpoint.conversation_id = snapshot.conversation_id
   AND checkpoint.turn_no = snapshot.latest_turn_no
   AND checkpoint.step_no = snapshot.latest_step_no
  LEFT JOIN execution_step step
    ON step.partner_id = checkpoint.partner_id
   AND step.id = checkpoint.execution_step_id
 WHERE conversation.partner_id = ? AND conversation.conversation_id = ?`,

	qSessionInsertCheckpoint: `
INSERT INTO step_checkpoint
       (partner_id, conversation_id, turn_no, step_no, execution_step_id, idempotency_key,
        state_uri, state_digest, fingerprint, input_tokens, output_tokens, tool_calls,
        cost_minor_units, currency_code)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,

	qSessionCreateSnapshot: `
INSERT INTO session_snapshot
       (partner_id, conversation_id, latest_turn_no, latest_step_no, state_uri, state_digest, revision)
VALUES (?, ?, ?, ?, ?, ?, 1)
ON CONFLICT DO NOTHING
RETURNING revision`,

	qSessionAdvanceSnapshot: `
UPDATE session_snapshot
   SET latest_turn_no = ?, latest_step_no = ?, state_uri = ?, state_digest = ?,
       revision = revision + 1, updated_at = CURRENT_TIMESTAMP
 WHERE partner_id = ? AND conversation_id = ? AND revision = ?
RETURNING revision`,

	// The oldest non-terminal turn is the executing one, whatever its state: a later turn never
	// runs while an earlier one is suspended or held.
	qSessionActiveTurn: `
SELECT turn.turn_no, COALESCE(snapshot.revision, 0), turn.request_id
  FROM conversation_turn turn
  LEFT JOIN session_snapshot snapshot
    ON snapshot.partner_id = turn.partner_id
   AND snapshot.conversation_id = turn.conversation_id
 WHERE turn.partner_id = ? AND turn.conversation_id = ?
   AND turn.status_code IN (` + sqlTurnLive + `)
 ORDER BY turn.turn_no
 LIMIT 1`,

	qSessionOwnerGuard: turnOwnerGuard,
	qSessionTurnFence: `
SELECT fence, status_code FROM conversation_turn WHERE partner_id = ? AND request_id = ?`,

	// The history row, when the turn has one, receives the result card in the same statement.
	qSessionCompleteTurn: `
WITH completed AS (
UPDATE conversation_turn turn
   SET status_code = ` + sqlTurnCompleted + `, response_uri = ?, response_digest = ?,
       started_at = COALESCE(started_at, CURRENT_TIMESTAMP), completed_at = CURRENT_TIMESTAMP
 WHERE turn.partner_id = ? AND turn.conversation_id = ? AND turn.turn_no = ?
   AND turn.request_id = ? AND turn.fence = ? AND turn.status_code IN (` + sqlTurnOwned + `)
   AND COALESCE((SELECT snapshot.revision
                   FROM session_snapshot snapshot
                  WHERE snapshot.partner_id = turn.partner_id
                    AND snapshot.conversation_id = turn.conversation_id), 0) = ?
RETURNING turn.partner_id, turn.conversation_id, turn.turn_no),
mirrored AS (
UPDATE conversation_turn_detail detail
   SET result_kind = ?, result_payload = ?, error_text = NULL
  FROM completed
 WHERE detail.partner_id = completed.partner_id AND detail.conversation_id = completed.conversation_id AND detail.turn_no = completed.turn_no
RETURNING detail.turn_no)
SELECT turn_no FROM completed`,

	qSessionCheckpointDigest: `
SELECT state_digest
  FROM step_checkpoint
 WHERE partner_id = ? AND conversation_id = ? AND turn_no = ? AND step_no = ?`,

	qSessionResponseDigest: `
SELECT response_digest
  FROM conversation_turn
 WHERE partner_id = ? AND conversation_id = ? AND turn_no = ?`,
}

// DurableSessionStore is the authoritative session store over conversation_turn,
// step_checkpoint, and session_snapshot; state bytes live in object storage and
// rows keep only URI+digest.
type DurableSessionStore struct {
	DB      port.DatabaseRepository
	Objects ObjectStateCodec
	// MaxResultCardBytes is the largest response copied into turn history; a larger one is
	// referenced, not copied. Default 64 KiB.
	MaxResultCardBytes int
	// Settlement writes a completing turn's budget and usage in its completion's transaction.
	Settlement TurnSettlementWriter

	once sync.Once
	qs   port.QueryService
}

// completionQueries are the statements of one completion: the session's and the usage events'.
var completionQueries = common.MergeMaps(durableSessionQueries, TurnLedgerQueries(), turnSettlementQueries)

func (store *DurableSessionStore) validate() error {
	if store.DB == nil || store.Objects == nil {
		return fmt.Errorf("%w: durable session store needs a database and an object codec", domain.ErrValidation)
	}
	if store.MaxResultCardBytes < 0 {
		return fmt.Errorf("%w: durable session store result card limit must not be negative", domain.ErrValidation)
	}
	return nil
}

func (store *DurableSessionStore) queries(ctx context.Context) port.QueryService {
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, durableSessionQueries) })
	return store.qs
}

// Load returns the latest snapshot with its state hydrated and digest-verified;
// a conversation without checkpoints loads at revision 0.
func (store *DurableSessionStore) Load(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, error) {
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" {
		return domain.SessionSnapshot{}, fmt.Errorf("%w: tenant and conversation are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return domain.SessionSnapshot{}, err
	}
	result, err := store.queries(ctx).Query(ctx, qSessionLoad, tenantID, conversationID)
	if err != nil {
		return domain.SessionSnapshot{}, fmt.Errorf("load session: %w", err)
	}
	if len(result.Rows) == 0 {
		return domain.SessionSnapshot{}, fmt.Errorf("%w: conversation %q", domain.ErrNotFound, conversationID)
	}
	row := result.Rows[0]
	if len(row) < 8 {
		return domain.SessionSnapshot{}, fmt.Errorf("decode session: expected 8 columns, got %d", len(row))
	}
	snapshot := domain.SessionSnapshot{ConversationID: conversationID, AgentVersion: common.AsString(row[0]), EndUserRef: common.AsString(row[7])}
	revision, hasSnapshot := common.AsInt64OK(row[5])
	if !hasSnapshot {
		return snapshot, nil
	}
	snapshot.LatestTurnNo = common.AsInt64(row[1])
	snapshot.LatestStepNo = int(common.AsInt64(row[2]))
	snapshot.StateRef = domain.ObjectRef{URI: common.AsString(row[3]), Digest: common.AsString(row[4])}
	snapshot.Revision = revision
	snapshot.LastCompletedStepID = common.AsString(row[6])
	if snapshot.State, err = store.Objects.Hydrate(ctx, snapshot.StateRef); err != nil {
		return domain.SessionSnapshot{}, fmt.Errorf("hydrate session %q revision %d: %w", conversationID, revision, err)
	}
	return snapshot, nil
}

// Checkpoint uploads the step state, then atomically guards the owner's turn, inserts the
// checkpoint row, and advances the snapshot from expectedRevision to expectedRevision+1;
// expectedRevision 0 creates the snapshot.
func (store *DurableSessionStore) Checkpoint(ctx context.Context, owner domain.TurnOwner, expectedRevision int64, checkpoint domain.StepCheckpoint) error {
	if err := owner.Check(); err != nil {
		return err
	}
	tenantID := owner.TenantID
	if err := validateCheckpoint(tenantID, expectedRevision, checkpoint); err != nil {
		return err
	}
	if err := store.validate(); err != nil {
		return err
	}
	ref, owned := checkpoint.StateRef, false
	if checkpoint.State != nil || ref.URI == "" {
		var err error
		if ref, err = store.Objects.Dehydrate(ctx, checkpointStateName(tenantID, checkpoint.ConversationID, checkpoint.TurnNo, checkpoint.StepNo), checkpoint.State); err != nil {
			return fmt.Errorf("dehydrate checkpoint state: %w", err)
		}
		owned = true
	}
	err := store.commitCheckpoint(context.WithoutCancel(ctx), owner, expectedRevision, checkpoint, ref)
	if err != nil && owned && !errors.Is(err, domain.ErrFenceLost) {
		return store.discardUnlessReferenced(ctx, ref, err, qSessionCheckpointDigest,
			tenantID, checkpoint.ConversationID, checkpoint.TurnNo, checkpoint.StepNo)
	}
	return err
}

func (store *DurableSessionStore) commitCheckpoint(ctx context.Context, owner domain.TurnOwner, expectedRevision int64, checkpoint domain.StepCheckpoint, ref domain.ObjectRef) error {
	tenantID := owner.TenantID
	tx, err := store.DB.BeginTx(ctx, durableSessionQueries)
	if err != nil {
		return fmt.Errorf("begin checkpoint: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	turnNo, err := guardOwnerTurn(ctx, tx, qSessionOwnerGuard, owner)
	if err != nil {
		return err
	}
	if turnNo != checkpoint.TurnNo {
		return fmt.Errorf("%w: the owner of turn %d cannot checkpoint turn %d", domain.ErrFenceLost, turnNo, checkpoint.TurnNo)
	}
	usage := checkpoint.Usage
	if _, err = tx.Query(ctx, qSessionInsertCheckpoint,
		tenantID, checkpoint.ConversationID, checkpoint.TurnNo, checkpoint.StepNo, checkpoint.ExecutionStepID,
		checkpoint.IdempotencyKey, ref.URI, ref.Digest, checkpoint.Fingerprint,
		usage.InputTokens, usage.OutputTokens, usage.ToolCalls, usage.CostMinorUnits, usage.Currency); err != nil {
		return fmt.Errorf("insert checkpoint: %w", err)
	}
	var advanced *keelmodel.QueryResult
	if expectedRevision == 0 {
		advanced, err = tx.Query(ctx, qSessionCreateSnapshot,
			tenantID, checkpoint.ConversationID, checkpoint.TurnNo, checkpoint.StepNo, ref.URI, ref.Digest)
	} else {
		advanced, err = tx.Query(ctx, qSessionAdvanceSnapshot,
			checkpoint.TurnNo, checkpoint.StepNo, ref.URI, ref.Digest,
			tenantID, checkpoint.ConversationID, expectedRevision)
	}
	if err != nil {
		return fmt.Errorf("advance snapshot: %w", err)
	}
	if len(advanced.Rows) == 0 {
		return fmt.Errorf("%w: session %q is not at revision %d", domain.ErrRevisionConflict, checkpoint.ConversationID, expectedRevision)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit checkpoint: %w", err)
	}
	committed = true
	return nil
}

// Complete stores the response and marks the owner's turn completed while it is the
// conversation's executing turn, its fence holds, and the snapshot is still at expectedRevision.
// The settlement commits in the same transaction, opened by the guarded touch of the owner's
// turn, so a reservation is settled exactly when its turn completes.
func (store *DurableSessionStore) Complete(ctx context.Context, owner domain.TurnOwner, conversationID string, expectedRevision int64, result domain.TurnResult, settlement domain.TurnSettlement) error {
	if err := owner.Check(); err != nil {
		return err
	}
	tenantID := owner.TenantID
	if expectedRevision < 0 || strings.TrimSpace(conversationID) == "" {
		return fmt.Errorf("%w: non-negative revision and conversation are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return err
	}
	if err := store.Settlement.validate(); err != nil {
		return err
	}
	active, err := store.queries(ctx).Query(ctx, qSessionActiveTurn, tenantID, conversationID)
	if err != nil {
		return fmt.Errorf("find active turn: %w", err)
	}
	if len(active.Rows) == 0 || len(active.Rows[0]) < 3 {
		return fmt.Errorf("%w: conversation %q has no executing turn", domain.ErrNotFound, conversationID)
	}
	turnNo, revision := common.AsInt64(active.Rows[0][0]), common.AsInt64(active.Rows[0][1])
	if activeRequest := common.AsString(active.Rows[0][2]); activeRequest != owner.RequestID {
		return fmt.Errorf("%w: turn %q is not the executing turn of conversation %q", domain.ErrFenceLost, owner.RequestID, conversationID)
	}
	if revision != expectedRevision {
		return fmt.Errorf("%w: session %q is at revision %d, expected %d", domain.ErrRevisionConflict, conversationID, revision, expectedRevision)
	}
	ref, err := store.Objects.Dehydrate(ctx, turnResponseName(tenantID, conversationID, turnNo), result.Response)
	if err != nil {
		return fmt.Errorf("dehydrate turn response: %w", err)
	}
	ctx = context.WithoutCancel(ctx)
	kind, card := resultCard(result.Response, ref, positiveOr(store.MaxResultCardBytes, defaultResultCardBytes))
	err = store.completeTx(ctx, owner, conversationID, turnNo, expectedRevision, ref, kind, card, settlement)
	if errors.Is(err, domain.ErrFenceLost) {
		return err
	}
	if err != nil {
		return store.discardUnlessReferenced(ctx, ref, err, qSessionResponseDigest, tenantID, conversationID, turnNo)
	}
	return nil
}

func (store *DurableSessionStore) completeTx(ctx context.Context, owner domain.TurnOwner, conversationID string, turnNo, expectedRevision int64, ref domain.ObjectRef, kind, card string, settlement domain.TurnSettlement) error {
	tx, err := store.DB.BeginTx(ctx, completionQueries)
	if err != nil {
		return fmt.Errorf("begin completion: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	guarded, err := guardOwnerTurn(ctx, tx, qSessionOwnerGuard, owner)
	if err != nil {
		return err
	}
	if guarded != turnNo {
		return fmt.Errorf("%w: the owner of turn %d cannot complete turn %d", domain.ErrFenceLost, guarded, turnNo)
	}
	unheld, err := store.Settlement.write(ctx, tx, owner.TenantID, owner.RequestID, conversationID, turnNo, settlement)
	if err != nil {
		return err
	}
	if unheld != nil {
		return unheld
	}
	completed, err := tx.Query(ctx, qSessionCompleteTurn,
		ref.URI, ref.Digest, owner.TenantID, conversationID, turnNo, owner.RequestID, owner.Fence, expectedRevision, kind, card)
	if err != nil {
		return fmt.Errorf("complete turn: %w", err)
	}
	if len(completed.Rows) == 0 {
		return store.refusedCompletion(ctx, tx, owner, turnNo, conversationID)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit completion: %w", err)
	}
	committed = true
	return nil
}

// refusedCompletion tells a lost fence apart from a snapshot that moved.
func (store *DurableSessionStore) refusedCompletion(ctx context.Context, tx port.TxQueryService, owner domain.TurnOwner, turnNo int64, conversationID string) error {
	current, err := tx.Query(ctx, qSessionTurnFence, owner.TenantID, owner.RequestID)
	if err != nil {
		return fmt.Errorf("read turn fence: %w", err)
	}
	if len(current.Rows) == 0 || common.AsInt64(current.Rows[0][0]) != owner.Fence ||
		!slices.Contains([]domain.TurnStatus{domain.TurnRunning, domain.TurnStreaming}, domain.TurnStatus(common.AsString(current.Rows[0][1]))) {
		return fmt.Errorf("%w: turn %q moved past fence %d", domain.ErrFenceLost, owner.RequestID, owner.Fence)
	}
	return fmt.Errorf("%w: turn %d of session %q changed before completion", domain.ErrRevisionConflict, turnNo, conversationID)
}

const defaultResultCardBytes = 64 << 10

// resultCard is the turn-history view of a response: JSON verbatim, anything else as text,
// and a reference to the stored object when the response is larger than maxBytes.
func resultCard(response []byte, ref domain.ObjectRef, maxBytes int) (kind, card string) {
	switch {
	case len(response) > maxBytes:
		encoded, _ := json.Marshal(map[string]string{"response_uri": ref.URI, "response_digest": ref.Digest})
		return "reference", string(encoded)
	case json.Valid(response):
		return "json", string(response)
	default:
		encoded, _ := json.Marshal(map[string]string{"text": string(response)})
		return "text", string(encoded)
	}
}

// A concurrent writer may already have committed a row pointing at the same
// content-addressed object, so the upload is deleted only once the row is known
// to point elsewhere; the reconciliation sweeper reclaims what remains.
func (store *DurableSessionStore) discardUnlessReferenced(ctx context.Context, ref domain.ObjectRef, cause error, digestQuery string, args ...any) error {
	ctx = context.WithoutCancel(ctx)
	referenced, err := store.queries(ctx).Query(ctx, digestQuery, args...)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("check object reference: %w", err))
	}
	if len(referenced.Rows) > 0 && common.AsString(referenced.Rows[0][0]) == ref.Digest {
		return cause
	}
	if err := store.Objects.Delete(ctx, ref); err != nil {
		return errors.Join(cause, fmt.Errorf("discard %s: %w", ref.URI, err))
	}
	return cause
}

func validateCheckpoint(tenantID, expectedRevision int64, checkpoint domain.StepCheckpoint) error {
	usage := checkpoint.Usage
	switch {
	case tenantID <= 0 || expectedRevision < 0:
		return fmt.Errorf("%w: tenant and non-negative expected revision are required", domain.ErrValidation)
	case strings.TrimSpace(checkpoint.ConversationID) == "" || len([]rune(checkpoint.ConversationID)) > 120:
		return fmt.Errorf("%w: checkpoint conversation id is required", domain.ErrValidation)
	case checkpoint.TurnNo <= 0 || checkpoint.StepNo <= 0 || checkpoint.ExecutionStepID <= 0:
		return fmt.Errorf("%w: checkpoint turn, step number, and execution step id must be positive", domain.ErrValidation)
	case strings.TrimSpace(checkpoint.IdempotencyKey) == "" || len([]rune(checkpoint.IdempotencyKey)) > 200:
		return fmt.Errorf("%w: checkpoint idempotency key is required", domain.ErrValidation)
	case len(checkpoint.Fingerprint) != 64:
		return fmt.Errorf("%w: checkpoint fingerprint must be a sha-256 hex digest", domain.ErrValidation)
	case len(usage.Currency) != 3:
		return fmt.Errorf("%w: checkpoint usage currency is required", domain.ErrValidation)
	case usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.CostMinorUnits < 0:
		return fmt.Errorf("%w: checkpoint usage must be non-negative", domain.ErrValidation)
	case checkpoint.State == nil && (checkpoint.StateRef.URI == "" || len(checkpoint.StateRef.Digest) != 64):
		return fmt.Errorf("%w: checkpoint needs state bytes or a dehydrated state reference", domain.ErrValidation)
	}
	return nil
}

var _ contract.DurableSessionStore = (*DurableSessionStore)(nil)
