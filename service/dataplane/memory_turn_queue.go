package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// MemoryTurnQueueConfig bounds one in-process turn queue.
type MemoryTurnQueueConfig struct {
	// Partitions is the fixed partition pool; ShardsPerTenant the subset one tenant spreads over.
	Partitions      int
	ShardsPerTenant int
	// MaxMessages bounds live (queued or leased) messages.
	MaxMessages int
	// MaxAttempts bounds deliveries before dead-lettering.
	MaxAttempts int
	// Weights supplies tenant weight and concurrency; nil means weight 1, unlimited.
	Weights contract.TenantWeightPolicy
	// Backoff computes the retry delay after a Nack; nil uses exponential seconds capped at one hour.
	Backoff func(attempt int) time.Duration
	// PriorityRank ranks a tenant's turns (lower runs first); nil ranks everything 0.
	PriorityRank func(domain.TenantContext) int
	Now          func() time.Time
}

// MemoryTurnQueue is the in-process reference dispatcher, fair scheduler, and
// dead-letter queue: the conformance target vendor adapters are checked against.
type MemoryTurnQueue struct {
	config MemoryTurnQueueConfig

	mu       sync.Mutex
	messages map[int64]*memoryQueueEntry
	byKey    map[streamKey]int64
	dead     []domain.QueueMessage
	reasons  []string
	nextID   int64
	nextTok  int64
	closed   bool
}

type memoryQueueEntry struct {
	id          int64
	dispatch    domain.TurnDispatch
	digest      string
	partition   int
	rank        int
	attempt     int
	status      string
	leaseToken  int64
	leaseUntil  time.Time
	workerID    string
	availableAt time.Time
	enqueuedAt  time.Time
}

var (
	_ contract.TurnDispatcher    = (*MemoryTurnQueue)(nil)
	_ contract.FairTurnScheduler = (*MemoryTurnQueue)(nil)
	_ contract.DeadLetterQueue   = (*MemoryTurnQueue)(nil)
)

// NewMemoryTurnQueue validates the configuration and builds the queue.
func NewMemoryTurnQueue(config MemoryTurnQueueConfig) (*MemoryTurnQueue, error) {
	if config.Partitions <= 0 || config.ShardsPerTenant <= 0 || config.ShardsPerTenant > config.Partitions {
		return nil, fmt.Errorf("%w: memory turn queue partitions and shards per tenant must be positive with shards <= partitions", domain.ErrValidation)
	}
	if config.MaxMessages <= 0 || config.MaxAttempts <= 0 {
		return nil, fmt.Errorf("%w: memory turn queue max messages and max attempts must be positive", domain.ErrValidation)
	}
	return &MemoryTurnQueue{
		config:   config,
		messages: make(map[int64]*memoryQueueEntry),
		byKey:    make(map[streamKey]int64),
	}, nil
}

func (queue *MemoryTurnQueue) now() time.Time {
	if queue.config.Now != nil {
		return queue.config.Now().UTC()
	}
	return time.Now().UTC()
}

func (queue *MemoryTurnQueue) backoff(attempt int) time.Duration {
	if queue.config.Backoff != nil {
		return queue.config.Backoff(attempt)
	}
	return defaultQueueBackoff(attempt)
}

// Enqueue accepts the turn once; an identical replay is a no-op.
func (queue *MemoryTurnQueue) Enqueue(ctx context.Context, dispatch domain.TurnDispatch) error {
	if err := validateDispatch(dispatch); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	turn := dispatch.Turn
	partition, err := ShufflePartition(turn.TenantContext.TenantID, turn.ConversationID, queue.config.Partitions, queue.config.ShardsPerTenant)
	if err != nil {
		return err
	}
	digest := DigestBytes(turn.Input)
	now := queue.now()
	if dispatch.EnqueuedAt.IsZero() {
		dispatch.EnqueuedAt = now
	}
	key := streamKey{turn.TenantContext.TenantID, turn.RequestID}

	queue.mu.Lock()
	defer queue.mu.Unlock()
	if err := queue.liveLocked(); err != nil {
		return err
	}
	if id, found := queue.byKey[key]; found {
		if queue.messages[id].digest != digest {
			return fmt.Errorf("%w: request %q is already queued with different input", domain.ErrConflict, turn.RequestID)
		}
		return nil
	}
	if len(queue.messages) >= queue.config.MaxMessages {
		return fmt.Errorf("%w: turn queue capacity reached", domain.ErrRateLimited)
	}
	rank := 0
	if queue.config.PriorityRank != nil {
		rank = queue.config.PriorityRank(turn.TenantContext)
	}
	queue.nextID++
	queue.messages[queue.nextID] = &memoryQueueEntry{
		id: queue.nextID, dispatch: dispatch, digest: digest, partition: partition, rank: rank,
		status: "queued", availableAt: dispatch.EnqueuedAt, enqueuedAt: dispatch.EnqueuedAt,
	}
	queue.byKey[key] = queue.nextID
	return nil
}

// Claim reclaims expired leases, then leases the head message of the fairest
// tenant; ErrNoReadyTurn means nothing is claimable now.
func (queue *MemoryTurnQueue) Claim(ctx context.Context, workerID string, leaseDuration time.Duration) (domain.QueueLease, error) {
	if strings.TrimSpace(workerID) == "" || leaseDuration <= 0 {
		return domain.QueueLease{}, fmt.Errorf("%w: worker id and positive lease duration are required", domain.ErrValidation)
	}
	if err := ctx.Err(); err != nil {
		return domain.QueueLease{}, err
	}
	now := queue.now()

	queue.mu.Lock()
	defer queue.mu.Unlock()
	if err := queue.liveLocked(); err != nil {
		return domain.QueueLease{}, err
	}
	queue.reclaimLocked(now)
	candidates := queue.candidatesLocked(now)
	ordered, err := pickTenants(ctx, queue.config.Weights, candidates)
	if err != nil {
		return domain.QueueLease{}, err
	}
	for _, candidate := range ordered {
		entry := queue.headLocked(candidate.tenantID, now)
		if entry == nil {
			continue
		}
		queue.nextTok++
		entry.status, entry.leaseToken, entry.leaseUntil, entry.workerID = "leased", queue.nextTok, now.Add(leaseDuration), workerID
		entry.attempt++
		return domain.QueueLease{Message: entry.message(), Deadline: entry.leaseUntil}, nil
	}
	return domain.QueueLease{}, ErrNoReadyTurn
}

// reclaimLocked returns expired leases to the queue and parks exhausted ones.
func (queue *MemoryTurnQueue) reclaimLocked(now time.Time) {
	for _, entry := range queue.messages {
		if entry.status != "leased" || entry.leaseUntil.After(now) {
			continue
		}
		if entry.attempt >= queue.config.MaxAttempts {
			queue.parkLocked(entry, "lease expired after last attempt")
			continue
		}
		entry.status, entry.leaseToken, entry.workerID = "queued", 0, ""
		entry.leaseUntil = time.Time{}
		entry.availableAt = now
	}
}

func (queue *MemoryTurnQueue) candidatesLocked(now time.Time) []tenantCandidate {
	byTenant := make(map[int64]*tenantCandidate)
	for _, entry := range queue.messages {
		tenantID := entry.dispatch.Turn.TenantContext.TenantID
		candidate := byTenant[tenantID]
		if candidate == nil {
			candidate = &tenantCandidate{tenantID: tenantID, bestRank: int(^uint(0) >> 1)}
			byTenant[tenantID] = candidate
		}
		if entry.status == "leased" {
			candidate.leased++
			continue
		}
		if entry.status != "queued" || entry.availableAt.After(now) {
			continue
		}
		candidate.oldest = minNonZero(candidate.oldest, entry.enqueuedAt.UnixNano())
		if entry.rank < candidate.bestRank {
			candidate.bestRank = entry.rank
		}
	}
	candidates := make([]tenantCandidate, 0, len(byTenant))
	for _, candidate := range byTenant {
		if candidate.oldest == 0 {
			continue
		}
		candidates = append(candidates, *candidate)
	}
	return candidates
}

// headLocked returns the tenant's next claimable message: nothing of a
// conversation is claimable while an older message of it is still live.
func (queue *MemoryTurnQueue) headLocked(tenantID int64, now time.Time) *memoryQueueEntry {
	var head *memoryQueueEntry
	for _, entry := range queue.messages {
		if entry.dispatch.Turn.TenantContext.TenantID != tenantID || entry.status != "queued" ||
			entry.availableAt.After(now) || entry.attempt >= queue.config.MaxAttempts {
			continue
		}
		if queue.blockedLocked(entry) {
			continue
		}
		if head == nil || entry.rank < head.rank ||
			(entry.rank == head.rank && (entry.enqueuedAt.Before(head.enqueuedAt) ||
				(entry.enqueuedAt.Equal(head.enqueuedAt) && entry.id < head.id))) {
			head = entry
		}
	}
	return head
}

func (queue *MemoryTurnQueue) blockedLocked(entry *memoryQueueEntry) bool {
	for _, other := range queue.messages {
		if other.id == entry.id ||
			other.dispatch.Turn.TenantContext.TenantID != entry.dispatch.Turn.TenantContext.TenantID ||
			other.dispatch.Turn.ConversationID != entry.dispatch.Turn.ConversationID {
			continue
		}
		if other.status == "leased" {
			return true
		}
		if other.status == "queued" && (other.enqueuedAt.Before(entry.enqueuedAt) ||
			(other.enqueuedAt.Equal(entry.enqueuedAt) && other.id < entry.id)) {
			return true
		}
	}
	return false
}

// Extend renews a live lease held by this worker.
func (queue *MemoryTurnQueue) Extend(ctx context.Context, messageID, workerID string, leaseDuration time.Duration) error {
	if leaseDuration <= 0 {
		return fmt.Errorf("%w: positive lease duration is required", domain.ErrValidation)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	entry, err := queue.leasedLocked(messageID, workerID)
	if err != nil {
		return err
	}
	entry.leaseUntil = queue.now().Add(leaseDuration)
	return nil
}

// Ack removes a completed turn from the queue.
func (queue *MemoryTurnQueue) Ack(ctx context.Context, messageID, workerID string) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	entry, err := queue.leasedLocked(messageID, workerID)
	if err != nil {
		return err
	}
	entry.status, entry.leaseToken, entry.workerID = "acked", 0, ""
	entry.leaseUntil = time.Time{}
	delete(queue.messages, entry.id)
	delete(queue.byKey, streamKey{entry.dispatch.Turn.TenantContext.TenantID, entry.dispatch.Turn.RequestID})
	return nil
}

// Nack requeues the turn with backoff, or dead-letters it once attempts are exhausted.
func (queue *MemoryTurnQueue) Nack(ctx context.Context, messageID, workerID, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: reason is required", domain.ErrValidation)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	entry, err := queue.leasedLocked(messageID, workerID)
	if err != nil {
		return err
	}
	if entry.attempt >= queue.config.MaxAttempts {
		queue.parkLocked(entry, reason)
		return nil
	}
	entry.status, entry.leaseToken, entry.workerID = "queued", 0, ""
	entry.leaseUntil = time.Time{}
	entry.availableAt = queue.now().Add(queue.backoff(entry.attempt))
	return nil
}

// Publish parks a message that will not be retried.
func (queue *MemoryTurnQueue) Publish(ctx context.Context, message domain.QueueMessage, reason string) error {
	if strings.TrimSpace(message.MessageID) == "" || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: message id and reason are required", domain.ErrValidation)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.dead = append(queue.dead, message)
	queue.reasons = append(queue.reasons, reason)
	return nil
}

// DeadLettered returns the parked messages and their reasons in park order.
func (queue *MemoryTurnQueue) DeadLettered() ([]domain.QueueMessage, []string) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return append([]domain.QueueMessage(nil), queue.dead...), append([]string(nil), queue.reasons...)
}

// Close drops queued state; it is idempotent and later operations fail.
func (queue *MemoryTurnQueue) Close() error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.closed = true
	queue.messages = make(map[int64]*memoryQueueEntry)
	queue.byKey = make(map[streamKey]int64)
	return nil
}

func (queue *MemoryTurnQueue) liveLocked() error {
	if queue.closed {
		return fmt.Errorf("%w: turn queue is closed", domain.ErrConflict)
	}
	return nil
}

func (queue *MemoryTurnQueue) leasedLocked(messageID, workerID string) (*memoryQueueEntry, error) {
	if err := queue.liveLocked(); err != nil {
		return nil, err
	}
	id, token, err := decodeMessageID(messageID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(workerID) == "" {
		return nil, fmt.Errorf("%w: worker id is required", domain.ErrValidation)
	}
	entry := queue.messages[id]
	if entry == nil || entry.status != "leased" || entry.leaseToken != token || entry.workerID != workerID {
		return nil, fmt.Errorf("%w: lease on message %s is not held by this worker", domain.ErrConflict, messageID)
	}
	return entry, nil
}

// parkLocked marks the entry dead and hands it to the dead-letter list.
func (queue *MemoryTurnQueue) parkLocked(entry *memoryQueueEntry, reason string) {
	entry.status, entry.leaseToken, entry.workerID = "dead", 0, ""
	entry.leaseUntil = time.Time{}
	queue.dead = append(queue.dead, entry.message())
	queue.reasons = append(queue.reasons, reason)
	delete(queue.messages, entry.id)
	delete(queue.byKey, streamKey{entry.dispatch.Turn.TenantContext.TenantID, entry.dispatch.Turn.RequestID})
}

func (entry *memoryQueueEntry) message() domain.QueueMessage {
	return domain.QueueMessage{
		MessageID: encodeMessageID(entry.id, entry.leaseToken),
		Dispatch:  entry.dispatch,
		Attempt:   entry.attempt,
	}
}

func minNonZero(current, candidate int64) int64 {
	if current == 0 || candidate < current {
		return candidate
	}
	return current
}
