package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ErrNoReadyTurn is returned by Claim when nothing is claimable right now.
var ErrNoReadyTurn = errors.New("no ready turn")

const (
	qSchedReclaim    = "scout_turn_queue_reclaim"
	qSchedExhausted  = "scout_turn_queue_exhausted"
	qSchedCandidates = "scout_turn_queue_candidates"
	qSchedClaim      = "scout_turn_queue_claim"
	qSchedExtend     = "scout_turn_queue_extend"
	qSchedAck        = "scout_turn_queue_ack"
	qSchedLockLeased = "scout_turn_queue_lock_leased"
	qSchedRetry      = "scout_turn_queue_retry"
	qSchedDead       = "scout_turn_queue_dead"
)

const queueRowColumns = `id, tenant_id, request_id, conversation_id, agent_id, reply_route,
       input_uri, input_digest, attempt, enqueued_at, lease_token, lease_until`

var turnSchedulerQueries = map[string]string{
	qSchedReclaim: `
UPDATE turn_queue
   SET status_code = 'queued', lease_token = NULL, lease_until = NULL, worker_id = NULL,
       last_error = 'lease expired'
 WHERE status_code = 'leased' AND lease_until < ? AND partition_no BETWEEN ? AND ?
	AND attempt < ?
RETURNING ` + queueRowColumns,

	qSchedExhausted: `
SELECT ` + queueRowColumns + `
  FROM turn_queue
 WHERE status_code = 'leased' AND lease_until < ? AND partition_no BETWEEN ? AND ? AND attempt >= ?
 ORDER BY lease_until, id
 FOR UPDATE SKIP LOCKED`,

	qSchedCandidates: `
SELECT tenant_id,
       SUM(CASE WHEN status_code = 'leased' THEN 1 ELSE 0 END),
       MIN(CASE WHEN status_code = 'queued' AND available_at <= ? THEN priority_rank END),
       MIN(CASE WHEN status_code = 'queued' AND available_at <= ? THEN enqueued_at END)
  FROM turn_queue
 WHERE status_code IN ('queued', 'leased') AND partition_no BETWEEN ? AND ?
 GROUP BY tenant_id
HAVING SUM(CASE WHEN status_code = 'queued' AND available_at <= ? THEN 1 ELSE 0 END) > 0`,

	// Head-of-line per conversation: nothing is claimable while an older
	// message of the same conversation is queued (even if backed off) or leased.
	qSchedClaim: `
UPDATE turn_queue
   SET status_code = 'leased', lease_token = ?, lease_until = ?, worker_id = ?, attempt = attempt + 1
 WHERE id = (SELECT q.id
               FROM turn_queue q
              WHERE q.tenant_id = ? AND q.status_code = 'queued' AND q.available_at <= ?
                AND q.partition_no BETWEEN ? AND ? AND q.attempt < ?
                AND NOT EXISTS (SELECT 1
                                  FROM turn_queue o
                                 WHERE o.tenant_id = q.tenant_id AND o.conversation_id = q.conversation_id
                                   AND o.id <> q.id
                                   AND (o.status_code = 'leased'
                                        OR (o.status_code = 'queued'
                                            AND (o.enqueued_at < q.enqueued_at
                                                 OR (o.enqueued_at = q.enqueued_at AND o.id < q.id)))))
              ORDER BY q.priority_rank, q.enqueued_at, q.id
              LIMIT 1
                FOR UPDATE SKIP LOCKED)
   AND status_code = 'queued'
RETURNING ` + queueRowColumns,

	qSchedExtend: `
UPDATE turn_queue
   SET lease_until = ?
 WHERE id = ? AND worker_id = ? AND lease_token = ? AND status_code = 'leased'
RETURNING id`,

	qSchedAck: `
UPDATE turn_queue
   SET status_code = 'acked', lease_token = NULL, lease_until = NULL, worker_id = NULL
 WHERE id = ? AND worker_id = ? AND lease_token = ? AND status_code = 'leased'
RETURNING id`,

	qSchedLockLeased: `
SELECT ` + queueRowColumns + `
  FROM turn_queue
 WHERE id = ? AND worker_id = ? AND lease_token = ? AND status_code = 'leased'
   FOR UPDATE`,

	qSchedRetry: `
UPDATE turn_queue
   SET status_code = 'queued', lease_token = NULL, lease_until = NULL, worker_id = NULL,
       available_at = ?, last_error = ?
 WHERE id = ? AND status_code = 'leased'
RETURNING id`,

	qSchedDead: `
UPDATE turn_queue
   SET status_code = 'dead', lease_token = NULL, lease_until = NULL, worker_id = NULL, last_error = ?
 WHERE id = ? AND status_code IN ('leased', 'queued')
RETURNING id`,
}

// QueueTurnScheduler leases turn_queue rows fairly: tenants are ordered by
// leased-per-weight (TenantWeightPolicy), each claim is a CAS with a lease
// token, and Extend/Ack/Nack are fenced by (message id, worker id, token).
type QueueTurnScheduler struct {
	DB port.DatabaseRepository
	// Objects hydrates the turn input referenced by the queue row.
	Objects ObjectStateCodec
	// Weights supplies tenant weight and concurrency; nil means weight 1, unlimited.
	Weights contract.TenantWeightPolicy
	// DeadLetters receives exhausted deliveries; a TxDeadLetterQueue publishes inside the dead-mark transaction.
	DeadLetters contract.DeadLetterQueue
	// MaxAttempts bounds deliveries before dead-lettering; required.
	MaxAttempts int
	// PartitionFrom/PartitionTo bound the partitions this worker serves (inclusive); both zero serves all.
	PartitionFrom int
	PartitionTo   int
	// Backoff computes the retry delay for a Nack at the given attempt; nil uses exponential seconds capped at one hour.
	Backoff func(attempt int) time.Duration
	Now     func() time.Time

	once sync.Once
	qs   port.QueryService
}

var _ contract.FairTurnScheduler = (*QueueTurnScheduler)(nil)

func (scheduler *QueueTurnScheduler) validate() error {
	if scheduler.DB == nil || scheduler.Objects == nil || scheduler.DeadLetters == nil {
		return fmt.Errorf("%w: turn scheduler needs a database, an object codec, and a dead letter queue", domain.ErrValidation)
	}
	if scheduler.MaxAttempts <= 0 || scheduler.PartitionFrom < 0 || scheduler.PartitionTo < scheduler.PartitionFrom {
		return fmt.Errorf("%w: turn scheduler max attempts must be positive and the partition range ordered", domain.ErrValidation)
	}
	return nil
}

func (scheduler *QueueTurnScheduler) queryMap() map[string]string {
	if txQueue, ok := scheduler.DeadLetters.(TxDeadLetterQueue); ok {
		return common.MergeMaps(txQueue.Queries(), turnSchedulerQueries)
	}
	return common.MergeMaps(turnSchedulerQueries)
}

func (scheduler *QueueTurnScheduler) queries(ctx context.Context) port.QueryService {
	scheduler.once.Do(func() { scheduler.qs = scheduler.DB.GetQueryService(ctx, scheduler.queryMap()) })
	return scheduler.qs
}

func (scheduler *QueueTurnScheduler) now() time.Time {
	if scheduler.Now != nil {
		return scheduler.Now().UTC()
	}
	return time.Now().UTC()
}

func (scheduler *QueueTurnScheduler) partitions() (int, int) {
	if scheduler.PartitionFrom == 0 && scheduler.PartitionTo == 0 {
		return 0, int(^uint16(0) >> 1)
	}
	return scheduler.PartitionFrom, scheduler.PartitionTo
}

func (scheduler *QueueTurnScheduler) backoff(attempt int) time.Duration {
	if scheduler.Backoff != nil {
		return scheduler.Backoff(attempt)
	}
	return defaultQueueBackoff(attempt)
}

// defaultQueueBackoff is exponential in seconds, capped at one hour.
func defaultQueueBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 12 {
		return time.Hour
	}
	return time.Duration(1<<uint(attempt)) * time.Second
}

// Claim reclaims expired leases in its partitions, dead-letters exhausted
// ones, then CAS-claims the head message of the fairest tenant.
func (scheduler *QueueTurnScheduler) Claim(ctx context.Context, workerID string, leaseDuration time.Duration) (domain.QueueLease, error) {
	if strings.TrimSpace(workerID) == "" || len([]rune(workerID)) > 120 || leaseDuration <= 0 {
		return domain.QueueLease{}, fmt.Errorf("%w: worker id and positive lease duration are required", domain.ErrValidation)
	}
	if err := scheduler.validate(); err != nil {
		return domain.QueueLease{}, err
	}
	now := scheduler.now()
	from, to := scheduler.partitions()
	if err := scheduler.reclaim(ctx, now, from, to); err != nil {
		return domain.QueueLease{}, err
	}
	candidates, err := scheduler.candidates(ctx, now, from, to)
	if err != nil {
		return domain.QueueLease{}, err
	}
	ordered, err := pickTenants(ctx, scheduler.Weights, candidates)
	if err != nil {
		return domain.QueueLease{}, err
	}
	token := scheduler.queries(ctx).GenID()
	deadline := now.Add(leaseDuration)
	for _, candidate := range ordered {
		claimed, err := scheduler.queries(ctx).Query(ctx, qSchedClaim,
			token, deadline, workerID,
			candidate.tenantID, now, from, to, scheduler.MaxAttempts)
		if err != nil {
			return domain.QueueLease{}, fmt.Errorf("claim turn: %w", err)
		}
		if len(claimed.Rows) == 0 {
			continue
		}
		message, err := scheduler.decodeMessage(ctx, claimed.Rows[0])
		if err != nil {
			return domain.QueueLease{}, err
		}
		return domain.QueueLease{Message: message, Deadline: deadline}, nil
	}
	return domain.QueueLease{}, ErrNoReadyTurn
}

func (scheduler *QueueTurnScheduler) reclaim(ctx context.Context, now time.Time, from, to int) error {
	ctx = context.WithoutCancel(ctx)
	_, err := scheduler.queries(ctx).Query(ctx, qSchedReclaim, now, from, to, scheduler.MaxAttempts)
	if err != nil {
		return fmt.Errorf("reclaim expired leases: %w", err)
	}
	// Exhausted rows stay leased until their dead-letter record and terminal
	// queue state commit together.
	return scheduler.deadLetterExpired(ctx, now, from, to)
}

func (scheduler *QueueTurnScheduler) deadLetterExpired(ctx context.Context, now time.Time, from, to int) error {
	tx, err := scheduler.DB.BeginTx(ctx, scheduler.queryMap())
	if err != nil {
		return fmt.Errorf("dead-letter expired turns: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	exhausted, err := tx.Query(ctx, qSchedExhausted, now, from, to, scheduler.MaxAttempts)
	if err != nil {
		return fmt.Errorf("list exhausted turns: %w", err)
	}
	for _, row := range exhausted.Rows {
		message, err := scheduler.decodeMessage(ctx, row)
		if err != nil {
			return err
		}
		if err := scheduler.deadLetterTx(ctx, tx, message, "lease expired after last attempt"); err != nil {
			return err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("dead-letter expired turns: commit: %w", err)
	}
	committed = true
	return nil
}

func (scheduler *QueueTurnScheduler) candidates(ctx context.Context, now time.Time, from, to int) ([]tenantCandidate, error) {
	result, err := scheduler.queries(ctx).Query(ctx, qSchedCandidates, now, now, from, to, now)
	if err != nil {
		return nil, fmt.Errorf("list ready tenants: %w", err)
	}
	candidates := make([]tenantCandidate, 0, len(result.Rows))
	for _, row := range result.Rows {
		if len(row) < 4 {
			return nil, fmt.Errorf("decode ready tenant: expected 4 columns, got %d", len(row))
		}
		candidate := tenantCandidate{tenantID: common.AsInt64(row[0]), leased: int(common.AsInt64(row[1])), bestRank: int(common.AsInt64(row[2]))}
		if oldest, ok := common.AsTimeOK(row[3]); ok {
			candidate.oldest = oldest.UnixNano()
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// Extend renews the lease deadline of a live claim.
func (scheduler *QueueTurnScheduler) Extend(ctx context.Context, messageID, workerID string, leaseDuration time.Duration) error {
	if err := scheduler.validate(); err != nil {
		return err
	}
	queueID, token, err := decodeMessageID(messageID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(workerID) == "" || leaseDuration <= 0 {
		return fmt.Errorf("%w: worker id and positive lease duration are required", domain.ErrValidation)
	}
	return scheduler.fenced(ctx, qSchedExtend, "extend", messageID, scheduler.now().Add(leaseDuration), queueID, workerID, token)
}

// Ack removes a completed turn from the queue; a lost lease is ErrConflict.
func (scheduler *QueueTurnScheduler) Ack(ctx context.Context, messageID, workerID string) error {
	if err := scheduler.validate(); err != nil {
		return err
	}
	queueID, token, err := decodeMessageID(messageID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(workerID) == "" {
		return fmt.Errorf("%w: worker id is required", domain.ErrValidation)
	}
	return scheduler.fenced(ctx, qSchedAck, "ack", messageID, queueID, workerID, token)
}

func (scheduler *QueueTurnScheduler) fenced(ctx context.Context, query, operation, messageID string, args ...any) error {
	result, err := scheduler.queries(ctx).Query(context.WithoutCancel(ctx), query, args...)
	if err != nil {
		return fmt.Errorf("%s turn %s: %w", operation, messageID, err)
	}
	if len(result.Rows) == 0 {
		return fmt.Errorf("%w: lease on message %s is not held by this worker", domain.ErrConflict, messageID)
	}
	return nil
}

// Nack requeues the turn with backoff, or dead-letters it and marks it dead in
// one transaction once MaxAttempts is reached.
func (scheduler *QueueTurnScheduler) Nack(ctx context.Context, messageID, workerID, reason string) error {
	if err := scheduler.validate(); err != nil {
		return err
	}
	queueID, token, err := decodeMessageID(messageID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(workerID) == "" || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: worker id and reason are required", domain.ErrValidation)
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := scheduler.DB.BeginTx(ctx, scheduler.queryMap())
	if err != nil {
		return fmt.Errorf("nack turn: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	locked, err := tx.Query(ctx, qSchedLockLeased, queueID, workerID, token)
	if err != nil {
		return fmt.Errorf("nack turn: lock: %w", err)
	}
	if len(locked.Rows) == 0 {
		return fmt.Errorf("%w: lease on message %s is not held by this worker", domain.ErrConflict, messageID)
	}
	message, err := scheduler.decodeMessage(ctx, locked.Rows[0])
	if err != nil {
		return err
	}
	reason = common.TruncateRunes(common.RedactForStorage(reason), 500)
	if message.Attempt >= scheduler.MaxAttempts {
		if err = scheduler.deadLetterTx(ctx, tx, message, reason); err != nil {
			return err
		}
	} else {
		retryAt := scheduler.now().Add(scheduler.backoff(message.Attempt))
		if _, err = tx.Query(ctx, qSchedRetry, retryAt, reason, queueID); err != nil {
			return fmt.Errorf("nack turn: retry: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("nack turn: commit: %w", err)
	}
	committed = true
	return nil
}

// deadLetterTx marks the row dead and parks it; a queue without transactional
// publish is written after the mark, so a crash between the two leaves a dead
// row without a parked copy, which the reconciliation sweep reports.
func (scheduler *QueueTurnScheduler) deadLetterTx(ctx context.Context, tx port.TxQueryService, message domain.QueueMessage, reason string) error {
	queueID, _, err := decodeMessageID(message.MessageID)
	if err != nil {
		return err
	}
	dead, err := tx.Query(ctx, qSchedDead, reason, queueID)
	if err != nil {
		return fmt.Errorf("mark turn dead: %w", err)
	}
	if len(dead.Rows) == 0 {
		return fmt.Errorf("%w: message %s is no longer live", domain.ErrConflict, message.MessageID)
	}
	if txQueue, ok := scheduler.DeadLetters.(TxDeadLetterQueue); ok {
		return txQueue.PublishTx(ctx, tx, message, reason)
	}
	return scheduler.DeadLetters.Publish(ctx, message, reason)
}

// decodeMessage builds a hydrated queue message from a queueRowColumns row.
func (scheduler *QueueTurnScheduler) decodeMessage(ctx context.Context, row []any) (domain.QueueMessage, error) {
	if len(row) < 12 {
		return domain.QueueMessage{}, fmt.Errorf("decode queue row: expected 12 columns, got %d", len(row))
	}
	ref := domain.ObjectRef{URI: common.AsString(row[6]), Digest: common.AsString(row[7])}
	input, err := scheduler.Objects.Hydrate(ctx, ref)
	if err != nil {
		return domain.QueueMessage{}, fmt.Errorf("hydrate turn input %s: %w", ref.URI, err)
	}
	message := domain.QueueMessage{
		MessageID: encodeMessageID(common.AsInt64(row[0]), common.AsInt64(row[10])),
		Dispatch: domain.TurnDispatch{
			Turn: domain.TurnRequest{
				TenantContext:  domain.TenantContext{TenantID: common.AsInt64(row[1])},
				RequestID:      common.AsString(row[2]),
				ConversationID: common.AsString(row[3]),
				AgentID:        common.AsString(row[4]),
				Input:          input,
			},
			ReplyRoute: common.AsString(row[5]),
		},
		Attempt: int(common.AsInt64(row[8])),
	}
	if enqueuedAt, ok := common.AsTimeOK(row[9]); ok {
		message.Dispatch.EnqueuedAt = enqueuedAt
	}
	return message, nil
}

// LeaseFromClaimRow adapts a keel LeasedQueueWorker claim row (queueRowColumns
// projection) into a hydrated lease for HandleJob.
func (scheduler *QueueTurnScheduler) LeaseFromClaimRow(ctx context.Context, row []any) (domain.QueueLease, error) {
	if err := scheduler.validate(); err != nil {
		return domain.QueueLease{}, err
	}
	message, err := scheduler.decodeMessage(ctx, row)
	if err != nil {
		return domain.QueueLease{}, err
	}
	lease := domain.QueueLease{Message: message}
	if until, ok := common.AsTimeOK(row[11]); ok {
		lease.Deadline = until
	}
	return lease, nil
}

// TurnQueueWorkerQueries returns pending/claim/reclaim named SQL for a keel
// LeasedQueueWorker over turn_queue (token first, id second in the claim), plus
// the query names in the order QueueQueries reports them. The loop-driven path
// orders by priority and age only; weighted fairness needs Claim.
func TurnQueueWorkerQueries(workerID string, lease time.Duration, batch, maxAttempts int) (queries map[string]string, pending, claim, reclaim string) {
	leaseSeconds := int(lease / time.Second)
	if leaseSeconds < 1 {
		leaseSeconds = 1
	}
	if batch < 1 {
		batch = 1
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	pending, claim, reclaim = "scout_turn_worker_pending", "scout_turn_worker_claim", "scout_turn_worker_reclaim"
	queries = map[string]string{
		pending: fmt.Sprintf(`
SELECT q.id
  FROM turn_queue q
 WHERE q.status_code = 'queued' AND q.available_at <= CURRENT_TIMESTAMP AND q.attempt < %d
   AND NOT EXISTS (SELECT 1 FROM turn_queue o
                    WHERE o.tenant_id = q.tenant_id AND o.conversation_id = q.conversation_id AND o.id <> q.id
                      AND (o.status_code = 'leased'
                           OR (o.status_code = 'queued' AND (o.enqueued_at < q.enqueued_at
                                                             OR (o.enqueued_at = q.enqueued_at AND o.id < q.id)))))
 ORDER BY q.priority_rank, q.enqueued_at, q.id
 LIMIT %d`, maxAttempts, batch),
		claim: fmt.Sprintf(`
UPDATE turn_queue
   SET status_code = 'leased', lease_token = ?, lease_until = CURRENT_TIMESTAMP + INTERVAL '%d seconds',
       worker_id = '%s', attempt = attempt + 1
 WHERE id = ? AND status_code = 'queued'
RETURNING `+queueRowColumns, leaseSeconds, strings.ReplaceAll(workerID, "'", "''")),
		reclaim: fmt.Sprintf(`
WITH expired AS (
    UPDATE turn_queue
       SET status_code = CASE WHEN attempt >= %d THEN 'dead' ELSE 'queued' END,
           lease_token = NULL, lease_until = NULL, worker_id = NULL,
           last_error = CASE WHEN attempt >= %d THEN 'lease expired after last attempt' ELSE 'lease expired' END
     WHERE status_code = 'leased' AND lease_until < CURRENT_TIMESTAMP
 RETURNING id, tenant_id, request_id, attempt, input_uri, input_digest, status_code
), parked AS (
    INSERT INTO turn_dead_letter (id, tenant_id, request_id, queue_id, reason, attempts, input_uri, input_digest)
    SELECT nextval('turn_dead_letter_seq'), tenant_id, request_id, id, 'lease expired after last attempt', attempt, input_uri, input_digest
      FROM expired
     WHERE status_code = 'dead'
    ON CONFLICT (queue_id) DO NOTHING
)
SELECT id FROM expired`, maxAttempts, maxAttempts),
	}
	return queries, pending, claim, reclaim
}

func encodeMessageID(queueID, token int64) string {
	return strconv.FormatInt(queueID, 10) + ":" + strconv.FormatInt(token, 10)
}

func decodeMessageID(messageID string) (queueID, token int64, err error) {
	left, right, found := strings.Cut(messageID, ":")
	if found {
		queueID, err = strconv.ParseInt(left, 10, 64)
		if err == nil {
			token, err = strconv.ParseInt(right, 10, 64)
		}
	}
	if !found || err != nil || queueID <= 0 {
		return 0, 0, fmt.Errorf("%w: message id %q is not <queue id>:<lease token>", domain.ErrValidation, messageID)
	}
	return queueID, token, nil
}
