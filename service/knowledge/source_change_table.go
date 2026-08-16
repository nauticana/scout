package knowledge

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qSourceEventInsert = "scout_knowledge_source_event_insert"
	qSourceEventPoll   = "scout_knowledge_source_event_poll"
	qSourceEventAck    = "scout_knowledge_source_event_ack"
)

var sourceEventQueries = map[string]string{
	qSourceEventInsert: `
INSERT INTO knowledge_source_event (id, tenant_id, knowledge_base_id, object_id, source_version, op_code, entitlements, occurred_at, acked_at)
VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP), NULL)`,
	qSourceEventPoll: `
SELECT tenant_id, knowledge_base_id, object_id, source_version, op_code, entitlements, occurred_at
  FROM knowledge_source_event
 WHERE tenant_id = ? AND knowledge_base_id = ? AND acked_at IS NULL
 ORDER BY occurred_at, id
 LIMIT ?`,
	qSourceEventAck: `
UPDATE knowledge_source_event
   SET acked_at = CURRENT_TIMESTAMP
 WHERE tenant_id = ? AND knowledge_base_id = ? AND object_id = ? AND source_version = ? AND op_code = ? AND acked_at IS NULL`,
}

// SourceChangeWriteQueries is merged into a producer's transaction query map so
// EnqueueSourceChange records the change atomically with the source write (outbox pattern).
func SourceChangeWriteQueries() map[string]string {
	return map[string]string{qSourceEventInsert: sourceEventQueries[qSourceEventInsert]}
}

// EnqueueSourceChange records one change event through the caller's query service or transaction.
func EnqueueSourceChange(ctx context.Context, qs keelport.QueryService, event domain.SourceChangeEvent) error {
	if err := validateSourceChange(event); err != nil {
		return err
	}
	if qs == nil {
		return fmt.Errorf("enqueue source change: query service is required")
	}
	var entitlements any
	if event.Entitlements != nil {
		entitlements = string(event.Entitlements)
	}
	var occurredAt any
	if !event.OccurredAt.IsZero() {
		occurredAt = event.OccurredAt.UTC()
	}
	_, err := qs.Query(ctx, qSourceEventInsert, qs.GenID(), event.TenantID, event.KnowledgeBaseID, event.ObjectID, event.SourceVersion, string(event.Op), entitlements, occurredAt)
	if err != nil {
		return fmt.Errorf("enqueue source change for %q: %w", event.ObjectID, err)
	}
	return nil
}

func validateSourceChange(event domain.SourceChangeEvent) error {
	if event.TenantID <= 0 || strings.TrimSpace(event.KnowledgeBaseID) == "" || strings.TrimSpace(event.ObjectID) == "" {
		return fmt.Errorf("%w: source change needs tenant, knowledge base, and object", domain.ErrValidation)
	}
	if event.Op != domain.SourceUpserted && event.Op != domain.SourceDeleted {
		return fmt.Errorf("%w: source change op %q is not supported", domain.ErrValidation, event.Op)
	}
	return nil
}

// TableSourceChangeSource is the CDC/outbox port over knowledge_source_event:
// producers enqueue, the ingestion worker polls in occurrence order and acks.
type TableSourceChangeSource struct {
	DB keelport.DatabaseRepository

	once sync.Once
	qs   keelport.QueryService
}

var _ contract.SourceChangeSource = (*TableSourceChangeSource)(nil)

func (source *TableSourceChangeSource) init(ctx context.Context) error {
	if source.DB == nil {
		return fmt.Errorf("source change source: database is required")
	}
	source.once.Do(func() { source.qs = source.DB.GetQueryService(ctx, sourceEventQueries) })
	if source.qs == nil {
		return fmt.Errorf("source change source: query service is required")
	}
	return nil
}

// Poll returns up to limit unacked events in occurrence order.
func (source *TableSourceChangeSource) Poll(ctx context.Context, tenantID int64, knowledgeBaseID string, limit int) ([]domain.SourceChangeEvent, error) {
	if tenantID <= 0 || strings.TrimSpace(knowledgeBaseID) == "" || limit <= 0 {
		return nil, fmt.Errorf("%w: tenant, knowledge base, and positive limit are required", domain.ErrValidation)
	}
	if err := source.init(ctx); err != nil {
		return nil, err
	}
	result, err := source.qs.Query(ctx, qSourceEventPoll, tenantID, knowledgeBaseID, limit)
	if err != nil {
		return nil, fmt.Errorf("poll source changes: %w", err)
	}
	events := make([]domain.SourceChangeEvent, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) < 7 {
			return nil, fmt.Errorf("poll source changes: expected 7 columns, got %d", len(row))
		}
		event := domain.SourceChangeEvent{
			TenantID: common.AsInt64(row[0]), KnowledgeBaseID: common.AsString(row[1]), ObjectID: common.AsString(row[2]),
			SourceVersion: common.AsString(row[3]), Op: domain.SourceChangeOp(common.AsString(row[4])), OccurredAt: common.AsTime(row[6]),
		}
		if row[5] != nil {
			event.Entitlements = []byte(common.AsString(row[5]))
		}
		events = append(events, event)
	}
	return events, nil
}

// Ack marks the events applied in one transaction; an already-acked event is a no-op.
func (source *TableSourceChangeSource) Ack(ctx context.Context, events []domain.SourceChangeEvent) error {
	if len(events) == 0 {
		return nil
	}
	for _, event := range events {
		if err := validateSourceChange(event); err != nil {
			return err
		}
	}
	if err := source.init(ctx); err != nil {
		return err
	}
	tx, err := source.DB.BeginTx(ctx, sourceEventQueries)
	if err != nil {
		return fmt.Errorf("ack source changes: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = keelport.RollbackDetached(tx)
		}
	}()
	for _, event := range events {
		if _, err = tx.Query(ctx, qSourceEventAck, event.TenantID, event.KnowledgeBaseID, event.ObjectID, event.SourceVersion, string(event.Op)); err != nil {
			return fmt.Errorf("ack source change for %q: %w", event.ObjectID, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("ack source changes: commit: %w", err)
	}
	committed = true
	return nil
}
