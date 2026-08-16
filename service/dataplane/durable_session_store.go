package dataplane

import (
	"context"
	"errors"
	"fmt"
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
)

var durableSessionQueries = map[string]string{
	qSessionLoad: `
SELECT conversation.agent_version, snapshot.latest_turn_no, snapshot.latest_step_no,
       snapshot.state_uri, snapshot.state_digest, snapshot.revision, step.step_id
  FROM agent_conversation conversation
  LEFT JOIN session_snapshot snapshot
    ON snapshot.tenant_id = conversation.tenant_id
   AND snapshot.conversation_id = conversation.conversation_id
  LEFT JOIN step_checkpoint checkpoint
    ON checkpoint.tenant_id = snapshot.tenant_id
   AND checkpoint.conversation_id = snapshot.conversation_id
   AND checkpoint.turn_no = snapshot.latest_turn_no
   AND checkpoint.step_no = snapshot.latest_step_no
  LEFT JOIN execution_step step
    ON step.id = checkpoint.execution_step_id
 WHERE conversation.tenant_id = ? AND conversation.conversation_id = ?`,

	qSessionInsertCheckpoint: `
INSERT INTO step_checkpoint
       (tenant_id, conversation_id, turn_no, step_no, execution_step_id, idempotency_key,
        state_uri, state_digest, fingerprint, input_tokens, output_tokens, tool_calls,
        cost_minor_units, currency_code)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,

	qSessionCreateSnapshot: `
INSERT INTO session_snapshot
       (tenant_id, conversation_id, latest_turn_no, latest_step_no, state_uri, state_digest, revision)
VALUES (?, ?, ?, ?, ?, ?, 1)
ON CONFLICT DO NOTHING
RETURNING revision`,

	qSessionAdvanceSnapshot: `
UPDATE session_snapshot
   SET latest_turn_no = ?, latest_step_no = ?, state_uri = ?, state_digest = ?,
       revision = revision + 1, updated_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND conversation_id = ? AND revision = ?
RETURNING revision`,

	// The oldest non-terminal turn is the executing one; later ones are queued.
	qSessionActiveTurn: `
SELECT turn.turn_no, COALESCE(snapshot.revision, 0)
  FROM conversation_turn turn
  LEFT JOIN session_snapshot snapshot
    ON snapshot.tenant_id = turn.tenant_id
   AND snapshot.conversation_id = turn.conversation_id
 WHERE turn.tenant_id = ? AND turn.conversation_id = ?
   AND turn.status_code IN ('queued', 'running', 'streaming')
 ORDER BY turn.turn_no
 LIMIT 1`,

	qSessionCompleteTurn: `
UPDATE conversation_turn turn
   SET status_code = 'completed', response_uri = ?, response_digest = ?,
       started_at = COALESCE(started_at, CURRENT_TIMESTAMP), completed_at = CURRENT_TIMESTAMP
 WHERE turn.tenant_id = ? AND turn.conversation_id = ? AND turn.turn_no = ?
   AND turn.status_code IN ('queued', 'running', 'streaming')
   AND COALESCE((SELECT snapshot.revision
                   FROM session_snapshot snapshot
                  WHERE snapshot.tenant_id = turn.tenant_id
                    AND snapshot.conversation_id = turn.conversation_id), 0) = ?
RETURNING turn.turn_no`,

	qSessionCheckpointDigest: `
SELECT state_digest
  FROM step_checkpoint
 WHERE tenant_id = ? AND conversation_id = ? AND turn_no = ? AND step_no = ?`,

	qSessionResponseDigest: `
SELECT response_digest
  FROM conversation_turn
 WHERE tenant_id = ? AND conversation_id = ? AND turn_no = ?`,
}

// DurableSessionStore is the authoritative session store over conversation_turn,
// step_checkpoint, and session_snapshot; state bytes live in object storage and
// rows keep only URI+digest.
type DurableSessionStore struct {
	DB      port.DatabaseRepository
	Objects ObjectStateCodec

	once sync.Once
	qs   port.QueryService
}

func (store *DurableSessionStore) validate() error {
	if store.DB == nil || store.Objects == nil {
		return fmt.Errorf("%w: durable session store needs a database and an object codec", domain.ErrValidation)
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
	if len(row) < 7 {
		return domain.SessionSnapshot{}, fmt.Errorf("decode session: expected 7 columns, got %d", len(row))
	}
	snapshot := domain.SessionSnapshot{ConversationID: conversationID, AgentVersion: common.AsString(row[0])}
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

// Checkpoint uploads the step state, then atomically inserts the checkpoint row
// and advances the snapshot from expectedRevision to expectedRevision+1;
// expectedRevision 0 creates the snapshot.
func (store *DurableSessionStore) Checkpoint(ctx context.Context, tenantID, expectedRevision int64, checkpoint domain.StepCheckpoint) error {
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
	err := store.commitCheckpoint(context.WithoutCancel(ctx), tenantID, expectedRevision, checkpoint, ref)
	if err != nil && owned {
		return store.discardUnlessReferenced(ctx, ref, err, qSessionCheckpointDigest,
			tenantID, checkpoint.ConversationID, checkpoint.TurnNo, checkpoint.StepNo)
	}
	return err
}

func (store *DurableSessionStore) commitCheckpoint(ctx context.Context, tenantID, expectedRevision int64, checkpoint domain.StepCheckpoint, ref domain.ObjectRef) error {
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

// Complete stores the response and marks the conversation's executing turn
// completed while the snapshot is still at expectedRevision.
func (store *DurableSessionStore) Complete(ctx context.Context, tenantID int64, conversationID string, expectedRevision int64, result domain.TurnResult) error {
	if tenantID <= 0 || expectedRevision < 0 || strings.TrimSpace(conversationID) == "" {
		return fmt.Errorf("%w: tenant, non-negative revision, and conversation are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return err
	}
	active, err := store.queries(ctx).Query(ctx, qSessionActiveTurn, tenantID, conversationID)
	if err != nil {
		return fmt.Errorf("find active turn: %w", err)
	}
	if len(active.Rows) == 0 || len(active.Rows[0]) < 2 {
		return fmt.Errorf("%w: conversation %q has no executing turn", domain.ErrNotFound, conversationID)
	}
	turnNo, revision := common.AsInt64(active.Rows[0][0]), common.AsInt64(active.Rows[0][1])
	if revision != expectedRevision {
		return fmt.Errorf("%w: session %q is at revision %d, expected %d", domain.ErrRevisionConflict, conversationID, revision, expectedRevision)
	}
	ref, err := store.Objects.Dehydrate(ctx, turnResponseName(tenantID, conversationID, turnNo), result.Response)
	if err != nil {
		return fmt.Errorf("dehydrate turn response: %w", err)
	}
	ctx = context.WithoutCancel(ctx)
	completed, err := store.queries(ctx).Query(ctx, qSessionCompleteTurn,
		ref.URI, ref.Digest, tenantID, conversationID, turnNo, expectedRevision)
	if err != nil {
		err = fmt.Errorf("complete turn: %w", err)
	} else if len(completed.Rows) == 0 {
		err = fmt.Errorf("%w: turn %d of session %q changed before completion", domain.ErrRevisionConflict, turnNo, conversationID)
	}
	if err != nil {
		return store.discardUnlessReferenced(ctx, ref, err, qSessionResponseDigest, tenantID, conversationID, turnNo)
	}
	return nil
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
