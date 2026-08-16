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

const qDeadLetterInsert = "scout_turn_dead_letter_insert"

var deadLetterQueries = map[string]string{
	// The input reference is copied from the queue row so the parked message
	// stays replayable even after the queue row is pruned.
	qDeadLetterInsert: `
INSERT INTO turn_dead_letter (id, tenant_id, request_id, queue_id, reason, attempts, input_uri, input_digest)
SELECT nextval('turn_dead_letter_seq'), q.tenant_id, q.request_id, q.id, ?, ?, q.input_uri, q.input_digest
  FROM turn_queue q
 WHERE q.id = ? AND q.tenant_id = ? AND q.request_id = ?
ON CONFLICT DO NOTHING
RETURNING id`,
}

// TxDeadLetterQueue is the optional capability of a dead-letter queue that can
// publish inside a caller's transaction, so parking and the queue's dead mark
// commit atomically.
type TxDeadLetterQueue interface {
	contract.DeadLetterQueue
	// Queries returns the named SQL the caller merges into its transaction map.
	Queries() map[string]string
	// PublishTx parks the message using the caller's query service.
	PublishTx(ctx context.Context, qs port.QueryService, message domain.QueueMessage, reason string) error
}

// TableDeadLetterQueue parks exhausted turn deliveries in turn_dead_letter,
// once per queue row.
type TableDeadLetterQueue struct {
	DB port.DatabaseRepository

	once sync.Once
	qs   port.QueryService
}

var _ TxDeadLetterQueue = (*TableDeadLetterQueue)(nil)

// Queries returns the dead-letter named SQL for transactional composition.
func (queue *TableDeadLetterQueue) Queries() map[string]string {
	return common.MergeMaps(deadLetterQueries)
}

func (queue *TableDeadLetterQueue) queries(ctx context.Context) port.QueryService {
	queue.once.Do(func() { queue.qs = queue.DB.GetQueryService(ctx, deadLetterQueries) })
	return queue.qs
}

// Publish parks the message on autocommit.
func (queue *TableDeadLetterQueue) Publish(ctx context.Context, message domain.QueueMessage, reason string) error {
	if queue.DB == nil {
		return fmt.Errorf("%w: dead letter queue needs a database", domain.ErrValidation)
	}
	return queue.PublishTx(context.WithoutCancel(ctx), queue.queries(ctx), message, reason)
}

// PublishTx parks the message inside the caller's transaction; a repeated
// publish of the same queue row is a no-op.
func (queue *TableDeadLetterQueue) PublishTx(ctx context.Context, qs port.QueryService, message domain.QueueMessage, reason string) error {
	queueID, _, err := decodeMessageID(message.MessageID)
	if err != nil {
		return err
	}
	tenantID := message.Dispatch.Turn.TenantContext.TenantID
	if qs == nil || tenantID <= 0 || strings.TrimSpace(message.Dispatch.Turn.RequestID) == "" || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: query service, tenant, request, and reason are required", domain.ErrValidation)
	}
	if _, err := qs.Query(ctx, qDeadLetterInsert,
		common.TruncateRunes(common.RedactForStorage(reason), 500), message.Attempt,
		queueID, tenantID, message.Dispatch.Turn.RequestID); err != nil {
		return fmt.Errorf("dead-letter turn %q: %w", message.Dispatch.Turn.RequestID, err)
	}
	return nil
}
