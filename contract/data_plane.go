package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// ConversationRuntime executes end-user turns in the data plane.
type ConversationRuntime interface {
	// HandleTurn executes one durably delivered turn against the conversation's pinned agent version
	// and publishes its frames to the dispatch reply route; a nil error means the delivery may be acked.
	HandleTurn(ctx context.Context, dispatch domain.TurnDispatch) (domain.TurnResult, error)
}

// ConversationIngress admits a turn and returns its asynchronous reply stream.
type ConversationIngress interface {
	// OpenTurn subscribes the reply route before durably dispatching the turn.
	OpenTurn(ctx context.Context, request domain.TurnRequest) (TurnReplySubscription, error)
}

// TurnDispatcher writes turns to shuffle-sharded durable partitions.
type TurnDispatcher interface {
	// Enqueue durably accepts a turn using its request ID for deduplication.
	Enqueue(ctx context.Context, dispatch domain.TurnDispatch) error
}

// FairTurnScheduler leases work using tenant weights and concurrency limits.
type FairTurnScheduler interface {
	// Claim leases the next fairly scheduled turn to a stateless worker.
	Claim(ctx context.Context, workerID string, leaseDuration time.Duration) (domain.QueueLease, error)
	// Extend renews a live lease while a worker is making progress.
	Extend(ctx context.Context, messageID, workerID string, leaseDuration time.Duration) error
	// Ack removes a successfully completed turn from the queue.
	Ack(ctx context.Context, messageID, workerID string) error
	// Nack releases a failed turn for bounded retry or dead-letter handling.
	Nack(ctx context.Context, messageID, workerID, reason string) error
}

// DurableSessionStore is the authoritative store for recoverable session state.
type DurableSessionStore interface {
	// Load returns the latest durable conversation snapshot.
	Load(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, error)
	// Checkpoint atomically persists a completed step with optimistic concurrency.
	Checkpoint(ctx context.Context, tenantID, expectedRevision int64, checkpoint domain.StepCheckpoint) error
	// Complete atomically records the final turn result.
	Complete(ctx context.Context, tenantID int64, conversationID string, expectedRevision int64, result domain.TurnResult) error
}

// HotSessionCache accelerates access to durable conversation snapshots.
type HotSessionCache interface {
	// Get returns a cached session and whether it was found.
	Get(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, bool, error)
	// Put caches a durable session snapshot for low-latency access.
	Put(ctx context.Context, tenantID int64, snapshot domain.SessionSnapshot) error
	// Invalidate removes a stale conversation snapshot.
	Invalidate(ctx context.Context, tenantID int64, conversationID string) error
}

// SessionCoordinator composes the hot cache with durable step checkpointing.
type SessionCoordinator interface {
	// Load returns the newest valid snapshot from the tiered session path.
	Load(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, error)
	// Checkpoint persists a step durably before refreshing the hot cache.
	Checkpoint(ctx context.Context, tenantID, expectedRevision int64, checkpoint domain.StepCheckpoint) error
	// Complete durably records the final result before refreshing the hot cache.
	Complete(ctx context.Context, tenantID int64, conversationID string, expectedRevision int64, result domain.TurnResult) error
}

// TurnReplyPublisher sends ordered worker response frames to conversation ingress.
type TurnReplyPublisher interface {
	// Publish is idempotent while a sequence is retained; an older ErrReplayExpired retry is success-equivalent.
	Publish(ctx context.Context, reply domain.TurnReply) error
}

// TurnReplySubscriber creates a short-lived response stream before dispatch.
type TurnReplySubscriber interface {
	// Subscribe opens the live reply stream.
	Subscribe(ctx context.Context, tenantID int64, requestID string) (TurnReplySubscription, error)
}

// ReplayTurnReplySubscriber can resume a reply stream from a retained sequence.
type ReplayTurnReplySubscriber interface {
	SubscribeFrom(ctx context.Context, tenantID int64, requestID string, fromSequence int64) (TurnReplySubscription, error)
}

// TurnCanceller stops a running turn without ending its conversation.
type TurnCanceller interface {
	// Cancel signals the worker executing the request to stop with a reason.
	Cancel(ctx context.Context, tenantID int64, requestID, reason string) error
}

// TurnReplySubscription receives ordered response frames for one request.
type TurnReplySubscription interface {
	// Route returns the opaque worker reply destination for this subscription.
	Route() string
	// Receive waits for the next reply frame.
	Receive(ctx context.Context) (domain.TurnReply, error)
	// Close releases the response subscription.
	Close() error
}

// StepIdempotencyStore makes interrupted step execution safely replayable. The
// step carries its compiled ExecutionStepID, which is the persisted identity.
type StepIdempotencyStore interface {
	// Begin claims the step or returns its committed result; an abandoned or lease-expired claim is replayable.
	Begin(ctx context.Context, tenantID int64, requestID string, step domain.ExecutionStep) (domain.StepResult, bool, error)
	// Commit durably binds a step result to its idempotency key.
	Commit(ctx context.Context, tenantID int64, requestID string, step domain.ExecutionStep, result domain.StepResult) error
	// Abandon releases an unfinished step so another worker can replay it.
	Abandon(ctx context.Context, tenantID int64, requestID string, step domain.ExecutionStep) error
}

// StepExecutor executes one node of an agent execution graph.
type StepExecutor interface {
	// Execute runs one graph step without owning orchestration state.
	Execute(ctx context.Context, input domain.StepInput) (domain.StepResult, error)
}

// StepExecutorRegistry resolves executors by graph step kind.
type StepExecutorRegistry interface {
	// ExecutorFor returns the executor registered for a graph step kind.
	ExecutorFor(ctx context.Context, stepKind string) (StepExecutor, error)
}

// DefinitionResolver retrieves the graph pinned to an agent version.
type DefinitionResolver interface {
	// Resolve returns a cached or persisted graph for a tenant-selected version.
	Resolve(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, error)
}

// DeadLetterQueue retains terminally failed turns for investigation.
type DeadLetterQueue interface {
	// Publish stores a terminally failed turn for investigation or replay.
	Publish(ctx context.Context, message domain.QueueMessage, reason string) error
}

// TenantWeightPolicy supplies each tenant's fair-scheduling weight and its
// concurrent-turn ceiling; a nil policy means weight 1 and no ceiling.
type TenantWeightPolicy interface {
	// SchedulingWeight returns the relative share (>= 1) and the maximum leased turns (0 = unlimited).
	SchedulingWeight(ctx context.Context, tenantID int64) (weight int, maxConcurrent int, err error)
}

// TurnBudgetEstimator quotes the tokens and cost to reserve for a turn before it
// runs; ingress and runtime quote the same request so the reservation replays.
type TurnBudgetEstimator interface {
	// Estimate returns the reservation-worthy usage priced in the tenant's budget currency.
	Estimate(ctx context.Context, request domain.TurnRequest) (domain.Usage, error)
}

// TurnRecordStore owns the durable turn identity keyed by (tenant, request id):
// ingress opens it before reserving budget, the runtime starts it, replays a
// finished one, and fails it on terminal errors; success is completed through
// DurableSessionStore.Complete.
type TurnRecordStore interface {
	// Open assigns the conversation's next turn number on first use and returns it; a reused id with different input is ErrConflict.
	Open(ctx context.Context, request domain.TurnRequest, input domain.ObjectRef) (int64, error)
	// Find returns the turn number, its status code, and once terminal the stored payload: the response of a completed turn or the error code of a failed one; unknown ids are ErrNotFound.
	Find(ctx context.Context, tenantID int64, requestID string) (turnNo int64, status string, payload []byte, err error)
	// Start marks a queued turn running; it is a no-op once running or terminal.
	Start(ctx context.Context, tenantID int64, requestID string) error
	// Fail marks a live turn failed or cancelled with its error code once; a later Fail is a no-op.
	Fail(ctx context.Context, tenantID int64, requestID, status, errorCode string) error
	// RecordUsage writes the settled usage event for a turn once; a repeat is a no-op.
	RecordUsage(ctx context.Context, tenantID int64, conversationID string, turnNo int64, subjectRef string, usage domain.Usage) error
}
