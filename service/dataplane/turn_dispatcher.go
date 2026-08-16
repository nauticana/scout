package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qQueueEnqueue    = "scout_turn_queue_enqueue"
	qQueueFindByReq  = "scout_turn_queue_find_by_request"
	qQueueTurnDigest = "scout_turn_queue_turn_digest"
)

// Enqueue copies the input reference from the opened turn record so the queue
// never stores a payload the durable turn identity does not already own.
var turnDispatcherQueries = map[string]string{
	qQueueEnqueue: `
INSERT INTO turn_queue
       (id, tenant_id, request_id, conversation_id, agent_id, partition_no, priority_rank,
        reply_route, input_uri, input_digest, attempt, status_code, available_at, enqueued_at)
SELECT nextval('turn_queue_seq'), turn.tenant_id, turn.request_id, turn.conversation_id, ?, ?, ?,
       ?, turn.input_uri, turn.input_digest, 0, 'queued', ?, ?
  FROM conversation_turn turn
 WHERE turn.tenant_id = ? AND turn.request_id = ? AND turn.conversation_id = ? AND turn.input_digest = ?
ON CONFLICT DO NOTHING
RETURNING id`,

	qQueueFindByReq: `
SELECT id, input_digest, status_code
  FROM turn_queue
 WHERE tenant_id = ? AND request_id = ?`,

	qQueueTurnDigest: `
SELECT input_digest, conversation_id
  FROM conversation_turn
 WHERE tenant_id = ? AND request_id = ?`,
}

// QueueTurnDispatcher writes admitted turns to turn_queue: shuffle-sharded by
// tenant, ordered per conversation, deduplicated by request id.
type QueueTurnDispatcher struct {
	DB port.DatabaseRepository
	// Partitions is the fixed partition pool size; ShardsPerTenant the subset one tenant spreads over.
	Partitions      int
	ShardsPerTenant int
	// PriorityRank maps the tenant's scheduling class to a rank (lower runs first); nil ranks everything 0.
	PriorityRank func(domain.TenantContext) int
	Now          func() time.Time

	once sync.Once
	qs   port.QueryService
}

var _ contract.TurnDispatcher = (*QueueTurnDispatcher)(nil)

func (dispatcher *QueueTurnDispatcher) validate() error {
	if dispatcher.DB == nil {
		return fmt.Errorf("%w: turn dispatcher needs a database", domain.ErrValidation)
	}
	if dispatcher.Partitions <= 0 || dispatcher.ShardsPerTenant <= 0 || dispatcher.ShardsPerTenant > dispatcher.Partitions {
		return fmt.Errorf("%w: turn dispatcher partitions and shards per tenant must be positive with shards <= partitions", domain.ErrValidation)
	}
	return nil
}

func (dispatcher *QueueTurnDispatcher) queries(ctx context.Context) port.QueryService {
	dispatcher.once.Do(func() { dispatcher.qs = dispatcher.DB.GetQueryService(ctx, turnDispatcherQueries) })
	return dispatcher.qs
}

func (dispatcher *QueueTurnDispatcher) now() time.Time {
	if dispatcher.Now != nil {
		return dispatcher.Now()
	}
	return time.Now()
}

// Enqueue durably accepts the turn once; an identical replay is a no-op and a
// reused request id with different input is ErrConflict.
func (dispatcher *QueueTurnDispatcher) Enqueue(ctx context.Context, dispatch domain.TurnDispatch) error {
	if err := validateDispatch(dispatch); err != nil {
		return err
	}
	if err := dispatcher.validate(); err != nil {
		return err
	}
	turn := dispatch.Turn
	tenantID := turn.TenantContext.TenantID
	partition, err := ShufflePartition(tenantID, turn.ConversationID, dispatcher.Partitions, dispatcher.ShardsPerTenant)
	if err != nil {
		return err
	}
	rank := 0
	if dispatcher.PriorityRank != nil {
		rank = dispatcher.PriorityRank(turn.TenantContext)
	}
	digest := DigestBytes(turn.Input)
	enqueuedAt := dispatch.EnqueuedAt
	if enqueuedAt.IsZero() {
		enqueuedAt = dispatcher.now()
	}
	ctx = context.WithoutCancel(ctx)
	inserted, err := dispatcher.queries(ctx).Query(ctx, qQueueEnqueue,
		turn.AgentID, partition, rank, dispatch.ReplyRoute, enqueuedAt.UTC(), enqueuedAt.UTC(),
		tenantID, turn.RequestID, turn.ConversationID, digest)
	if err != nil {
		return fmt.Errorf("enqueue turn %q: %w", turn.RequestID, err)
	}
	if len(inserted.Rows) > 0 {
		return nil
	}
	existing, err := dispatcher.queries(ctx).Query(ctx, qQueueFindByReq, tenantID, turn.RequestID)
	if err != nil {
		return fmt.Errorf("enqueue turn %q: find existing: %w", turn.RequestID, err)
	}
	if len(existing.Rows) > 0 {
		if common.AsString(existing.Rows[0][1]) != digest {
			return fmt.Errorf("%w: request %q is already queued with different input", domain.ErrConflict, turn.RequestID)
		}
		return nil
	}
	record, err := dispatcher.queries(ctx).Query(ctx, qQueueTurnDigest, tenantID, turn.RequestID)
	if err != nil {
		return fmt.Errorf("enqueue turn %q: find turn record: %w", turn.RequestID, err)
	}
	if len(record.Rows) == 0 {
		return fmt.Errorf("%w: turn record for request %q must be opened before dispatch", domain.ErrNotFound, turn.RequestID)
	}
	return fmt.Errorf("%w: request %q turn record has different input or conversation", domain.ErrConflict, turn.RequestID)
}

func validateDispatch(dispatch domain.TurnDispatch) error {
	turn := dispatch.Turn
	if turn.TenantContext.TenantID <= 0 || strings.TrimSpace(turn.RequestID) == "" || len([]rune(turn.RequestID)) > 120 ||
		strings.TrimSpace(turn.ConversationID) == "" || strings.TrimSpace(turn.AgentID) == "" ||
		strings.TrimSpace(dispatch.ReplyRoute) == "" || len([]rune(dispatch.ReplyRoute)) > 400 {
		return fmt.Errorf("%w: tenant, request, conversation, agent, and reply route are required", domain.ErrValidation)
	}
	return nil
}
