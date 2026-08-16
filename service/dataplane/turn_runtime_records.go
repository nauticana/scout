package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qRecordLock  = "scout_turn_record_lock"
	qRecordFind  = "scout_turn_record_find"
	qRecordOpen  = "scout_turn_record_open"
	qRecordStart = "scout_turn_record_start"
	qRecordFail  = "scout_turn_record_fail"
)

var turnRecordQueries = map[string]string{
	qRecordLock: `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`,

	qRecordFind: `
SELECT turn_no, conversation_id, status_code, input_digest, response_uri, response_digest
  FROM conversation_turn
 WHERE tenant_id = ? AND request_id = ?`,

	qRecordOpen: `
INSERT INTO conversation_turn
       (tenant_id, conversation_id, turn_no, request_id, status_code, input_uri, input_digest)
SELECT ?, ?, COALESCE(MAX(turn_no), 0) + 1, ?, 'queued', ?, ?
  FROM conversation_turn
 WHERE tenant_id = ? AND conversation_id = ?
ON CONFLICT DO NOTHING
RETURNING turn_no`,

	qRecordStart: `
UPDATE conversation_turn
   SET status_code = 'running', started_at = COALESCE(started_at, CURRENT_TIMESTAMP)
 WHERE tenant_id = ? AND request_id = ? AND status_code = 'queued'
RETURNING turn_no`,

	qRecordFail: `
UPDATE conversation_turn
   SET status_code = ?, response_uri = ?, response_digest = ?,
       started_at = COALESCE(started_at, CURRENT_TIMESTAMP), completed_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND request_id = ? AND status_code IN ('queued', 'running', 'streaming')
RETURNING turn_no`,
}

// TableTurnRecordStore is the TurnRecordStore over conversation_turn: the
// request-keyed turn identity, its lifecycle status, and one usage event per
// settled turn (through the TurnLedger insert). Failure payloads (the error
// code) go through the object codec like responses do.
type TableTurnRecordStore struct {
	DB      port.DatabaseRepository
	Objects ObjectStateCodec
	// UsageCategory labels the usage event written per settled turn.
	UsageCategory string

	once sync.Once
	qs   port.QueryService
}

var _ contract.TurnRecordStore = (*TableTurnRecordStore)(nil)

func (store *TableTurnRecordStore) validate() error {
	if store.DB == nil || store.Objects == nil || strings.TrimSpace(store.UsageCategory) == "" {
		return fmt.Errorf("%w: turn record store needs a database, an object codec, and a usage category", domain.ErrValidation)
	}
	return nil
}

func (store *TableTurnRecordStore) queryMap() map[string]string {
	return common.MergeMaps(TurnLedgerQueries(), turnRecordQueries)
}

func (store *TableTurnRecordStore) queries(ctx context.Context) port.QueryService {
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, store.queryMap()) })
	return store.qs
}

type turnRecordRow struct {
	turnNo         int64
	conversationID string
	status         string
	inputDigest    string
	response       domain.ObjectRef
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
	if len(row) < 6 {
		return turnRecordRow{}, false, fmt.Errorf("decode turn record: expected 6 columns, got %d", len(row))
	}
	return turnRecordRow{
		turnNo: common.AsInt64(row[0]), conversationID: common.AsString(row[1]),
		status: common.AsString(row[2]), inputDigest: common.AsString(row[3]),
		response: domain.ObjectRef{URI: common.AsString(row[4]), Digest: common.AsString(row[5])},
	}, true, nil
}

// Open inserts the turn under a per-conversation advisory lock so turn numbers
// stay dense; a replay of the same request id returns the existing turn.
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
	inserted, err := tx.Query(ctx, qRecordOpen,
		tenantID, request.ConversationID, request.RequestID, input.URI, input.Digest,
		tenantID, request.ConversationID)
	if err != nil {
		return 0, fmt.Errorf("open turn: insert: %w", err)
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
func (store *TableTurnRecordStore) Find(ctx context.Context, tenantID int64, requestID string) (int64, string, []byte, error) {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return 0, "", nil, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return 0, "", nil, err
	}
	row, found, err := store.find(ctx, tenantID, requestID)
	if err != nil {
		return 0, "", nil, err
	}
	if !found {
		return 0, "", nil, fmt.Errorf("%w: turn request %q", domain.ErrNotFound, requestID)
	}
	if row.response.URI == "" {
		return row.turnNo, row.status, nil, nil
	}
	payload, err := store.Objects.Hydrate(ctx, row.response)
	if err != nil {
		return 0, "", nil, fmt.Errorf("hydrate turn %q payload: %w", requestID, err)
	}
	return row.turnNo, row.status, payload, nil
}

// Start moves a queued turn to running.
func (store *TableTurnRecordStore) Start(ctx context.Context, tenantID int64, requestID string) error {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return err
	}
	if _, err := store.queries(ctx).Query(context.WithoutCancel(ctx), qRecordStart, tenantID, requestID); err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	return nil
}

// Fail stores the error code as the turn's terminal payload; an already
// terminal turn is left untouched.
func (store *TableTurnRecordStore) Fail(ctx context.Context, tenantID int64, requestID, status, errorCode string) error {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" || (status != "failed" && status != "cancelled") || strings.TrimSpace(errorCode) == "" {
		return fmt.Errorf("%w: tenant, request, failed or cancelled status, and error code are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return err
	}
	ref, err := store.Objects.Dehydrate(ctx, turnFailureName(tenantID, requestID), []byte(errorCode))
	if err != nil {
		return fmt.Errorf("dehydrate turn failure: %w", err)
	}
	ctx = context.WithoutCancel(ctx)
	if _, err = store.queries(ctx).Query(ctx, qRecordFail, status, ref.URI, ref.Digest, tenantID, requestID); err != nil {
		return fmt.Errorf("fail turn: %w", err)
	}
	return nil
}

// RecordUsage writes the settled usage event through the ledger's insert, which
// is unique per (turn, category).
func (store *TableTurnRecordStore) RecordUsage(ctx context.Context, tenantID int64, conversationID string, turnNo int64, subjectRef string, usage domain.Usage) error {
	if tenantID <= 0 || strings.TrimSpace(conversationID) == "" || turnNo <= 0 || len(usage.Currency) != 3 ||
		usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CostMinorUnits < 0 {
		return fmt.Errorf("%w: turn identity and non-negative priced usage are required", domain.ErrValidation)
	}
	if err := store.validate(); err != nil {
		return err
	}
	ledger := TurnLedger{UsageCategory: store.UsageCategory}
	_, err := ledger.InsertUsageEvent(context.WithoutCancel(ctx), store.queries(ctx), tenantID, conversationID, turnNo, subjectRef, usage)
	return err
}

func turnInputName(tenantID int64, requestID string) string {
	return fmt.Sprintf("turn-input/%d/%s", tenantID, requestID)
}

func turnFailureName(tenantID int64, requestID string) string {
	return fmt.Sprintf("turn-failure/%d/%s", tenantID, requestID)
}

func isTerminalTurnStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}
