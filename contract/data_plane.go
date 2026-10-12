package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// ConversationRuntime executes end-user turns in the data plane.
type ConversationRuntime interface {
	// HandleTurn acquires the turn with the delivery's current queue claim, executes it against the
	// conversation's pinned agent version, and publishes its frames to the dispatch reply route. A nil
	// error means the delivery may be acked; domain.ErrFenceLost means the claim no longer owns the
	// turn, so the delivery is dropped without a retry.
	HandleTurn(ctx context.Context, claim domain.TurnClaim) (domain.TurnResult, error)
}

// ConversationIngress admits a turn and returns its asynchronous reply stream.
type ConversationIngress interface {
	// OpenTurn subscribes the reply route before durably dispatching the turn.
	OpenTurn(ctx context.Context, request domain.TurnRequest) (TurnReplySubscription, error)
	// Selectable lists the agents of an agent type alias that admit work and that person may address.
	Selectable(ctx context.Context, tenant domain.TenantContext, person domain.PrincipalRef, aliasID string) ([]domain.SelectableAgent, error)
}

// TurnDispatcher writes turns to shuffle-sharded durable partitions.
type TurnDispatcher interface {
	// Enqueue durably accepts a turn using its request ID for deduplication.
	Enqueue(ctx context.Context, dispatch domain.TurnDispatch) error
}

// TurnFeedbackRecorder records a person's rating, 1 to 5, of a completed turn they may
// converse about; a second rating by the same person replaces the first.
type TurnFeedbackRecorder interface {
	RecordFeedback(ctx context.Context, tenant domain.TenantContext, person domain.PrincipalRef, requestID string, rating int, reason string) error
}

// TurnRequeuer returns the acknowledged delivery of a resumed turn to the queue, so a
// worker replays it from its last checkpoint. A repeat is a no-op.
type TurnRequeuer interface {
	Requeue(ctx context.Context, tenantID int64, requestID string) error
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

// ClaimedTurnScheduler hydrates the row returned by keel's leased queue loop.
// It is the worker-facing extension of the transport-neutral fair scheduler.
type ClaimedTurnScheduler interface {
	FairTurnScheduler
	LeaseFromClaimRow(ctx context.Context, row []any) (domain.QueueLease, error)
}

// DurableSessionStore is the authoritative store for recoverable session state. Its writes
// are made by the turn's owner and refused with domain.ErrFenceLost once its fence moved on.
type DurableSessionStore interface {
	// Load returns the latest durable conversation snapshot.
	Load(ctx context.Context, tenantID int64, conversationID string) (domain.SessionSnapshot, error)
	// Checkpoint atomically persists a completed step with optimistic concurrency.
	Checkpoint(ctx context.Context, owner domain.TurnOwner, expectedRevision int64, checkpoint domain.StepCheckpoint) error
	// Complete atomically records the final result of the owner's turn with its settlement: the
	// reservation and usage events commit in the same transaction, or nothing does.
	Complete(ctx context.Context, owner domain.TurnOwner, conversationID string, expectedRevision int64, result domain.TurnResult, settlement domain.TurnSettlement) error
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
	Checkpoint(ctx context.Context, owner domain.TurnOwner, expectedRevision int64, checkpoint domain.StepCheckpoint) error
	// Complete durably records the final result with its settlement before refreshing the hot cache.
	Complete(ctx context.Context, owner domain.TurnOwner, conversationID string, expectedRevision int64, result domain.TurnResult, settlement domain.TurnSettlement) error
}

// TurnReplyPublisher sends ordered worker response frames to conversation ingress.
type TurnReplyPublisher interface {
	// Publish is idempotent while a sequence is retained; an older ErrReplayExpired retry is success-equivalent.
	// It advances the stream's (fence, sequence) atomically: a frame below the stream's fence is
	// domain.ErrFenceLost, and only a higher fence may replace the frame at the stream's head.
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

// StoredTurnReplyReader rebuilds the terminal frame from the durable turn record
// after the short-lived reply buffer can no longer satisfy a replay cursor.
type StoredTurnReplyReader interface {
	StoredReply(ctx context.Context, tenantID int64, requestID string, sequence int64) (domain.TurnReply, error)
}

// TurnCanceller stops a running turn without ending its conversation.
type TurnCanceller interface {
	// Cancel signals the worker executing the request to stop with a reason.
	Cancel(ctx context.Context, tenantID int64, requestID, reason string) error
}

// TurnControlWatcher lets the process executing a turn observe control requested anywhere.
type TurnControlWatcher interface {
	// Watch derives the owner's turn context: a cancellation fails it with domain.ErrTurnCanceled
	// as its cause and a fence other than the owner's with domain.ErrFenceLost. A hold request
	// does not cancel; the watch reports it. The watch must be released when the turn ends.
	Watch(ctx context.Context, owner domain.TurnOwner) (TurnWatch, error)
}

// TurnWatch is one running turn's view of its control requests.
type TurnWatch interface {
	Context() context.Context
	// HoldRequested reports whether a hold was requested at the owner's fence.
	HoldRequested() bool
	Release()
}

// TurnControl holds and releases turns for whoever operates them. Each transition moves the
// turn's fence, which fences out a worker that still runs it.
type TurnControl interface {
	// RequestHold holds a queued or suspended turn at once under code and asks a running one to
	// hold at its next durable checkpoint; the first request's reason stands. A turn already held
	// reports TurnHeld, and an ended or cancelled one is domain.ErrNotFound.
	RequestHold(ctx context.Context, tenantID int64, requestID string, code domain.TurnHoldCode, reason string) (domain.TurnHoldOutcome, error)
	// Preempt holds any live, uncancelled turn at once under domain.HoldPreempted, so a running
	// worker's next fenced write is refused. A turn already held reports TurnHeld, and an ended or
	// cancelled one is domain.ErrNotFound.
	Preempt(ctx context.Context, tenantID int64, requestID string) (domain.TurnHoldOutcome, error)
	// Release returns a turn held at expectedFence to the queue and returns its new fence; any
	// other state or fence, or a call of the turn whose outcome is still open, is domain.ErrConflict.
	Release(ctx context.Context, tenantID int64, requestID string, expectedFence int64) (int64, error)
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
// step carries its compiled ExecutionStepID, which is the persisted identity. Every
// call is made by the turn's owner and refused with domain.ErrFenceLost once its fence moved on.
type StepIdempotencyStore interface {
	// Begin claims the step or returns its committed result; an abandoned or lease-expired claim is replayable.
	Begin(ctx context.Context, owner domain.TurnOwner, step domain.ExecutionStep) (domain.StepResult, bool, error)
	// Commit durably binds a step result to its idempotency key.
	Commit(ctx context.Context, owner domain.TurnOwner, step domain.ExecutionStep, result domain.StepResult) error
	// Abandon releases an unfinished step so another worker can replay it.
	Abandon(ctx context.Context, owner domain.TurnOwner, step domain.ExecutionStep) error
}

// StepExecutor executes one node of an agent execution graph.
type StepExecutor interface {
	// Execute runs one graph step without owning orchestration state.
	Execute(ctx context.Context, input domain.StepInput) (domain.StepResult, error)
}

// LoopJournal is the durable, append-only record of one model↔tool loop. It is
// what lets a redelivered or resumed loop continue instead of repeating a model
// decision or a committed tool effect.
type LoopJournal interface {
	// Load returns the entries in order; an unknown key is an empty journal.
	Load(ctx context.Context, key domain.LoopKey) ([]domain.LoopEntry, error)
	// Append stores entry at entry.EntryNo for the owner of the key's turn. The first writer
	// wins: the stored entry is returned, which is the caller's own unless a concurrent worker
	// got there first. An owner whose fence moved on is domain.ErrFenceLost.
	Append(ctx context.Context, owner domain.TurnOwner, key domain.LoopKey, entry domain.LoopEntry) (domain.LoopEntry, error)
}

// ToolLoopRequestBuilder builds the opening model request of a loop step from
// the step input. Prompt assembly is product behavior; the executor owns the
// tools, the conversation that follows, and the output constraint.
type ToolLoopRequestBuilder interface {
	Build(ctx context.Context, input domain.StepInput, config domain.ToolLoopConfig) (domain.ModelRequest, error)
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

// TenantWeightPolicy supplies each tenant's fair-scheduling weight and the
// concurrent-turn ceilings of the tenant and of each principal acting inside it;
// a nil policy means weight 1 and no ceiling. Ceilings gate selection, so racing
// workers may overshoot one by the number of workers claiming at once.
type TenantWeightPolicy interface {
	// SchedulingWeight returns the relative share (>= 1) and the maximum leased turns (0 = unlimited).
	SchedulingWeight(ctx context.Context, tenantID int64) (weight int, maxConcurrent int, err error)
	// PrincipalCeiling returns the maximum leased turns one principal may hold (0 = unlimited).
	// A principal at its ceiling is passed over; the tenant's other principals keep running.
	PrincipalCeiling(ctx context.Context, tenantID int64, principal domain.PrincipalRef) (maxConcurrent int, err error)
}

// TurnBudgetEstimator quotes the tokens and cost to reserve for a turn before it
// runs; ingress and runtime quote the same request so the reservation replays.
type TurnBudgetEstimator interface {
	// Estimate returns the reservation-worthy usage priced in the tenant's budget currency.
	Estimate(ctx context.Context, request domain.TurnRequest) (domain.Usage, error)
}

// TurnRecordStore owns the durable turn identity keyed by (tenant, request id):
// ingress opens it before reserving budget, the runtime acquires it, replays a
// finished one, and fails it on terminal errors; success is completed through
// DurableSessionStore.Complete. Either end writes the turn's settlement in its own
// transaction. Each acquisition moves the turn's fence, and the owner's writes are
// refused with domain.ErrFenceLost once it moved on.
type TurnRecordStore interface {
	// Address resolves the agent serving a turn: agentID when it is of aliasID's type, else the
	// agent the conversation already runs on, else the alias target.
	Address(ctx context.Context, tenantID int64, aliasID, agentID, conversationID string) (string, error)
	// Selectable lists the deployed agents of aliasID's type that admit work, the alias target first.
	Selectable(ctx context.Context, tenantID int64, aliasID string) ([]domain.SelectableAgent, error)
	// Open assigns the conversation's next turn number on first use and returns it; a reused id with different
	// input is ErrConflict, an agent that does not admit work refuses the turn, and a new turn beyond the
	// turns per minute of the conversation's release is ErrRateLimited with retry advice.
	Open(ctx context.Context, request domain.TurnRequest, input domain.ObjectRef) (int64, error)
	// Find returns the turn number, status, fence, and once terminal the stored payload: the response of a
	// completed turn or the error code of a failed one; unknown ids are ErrNotFound.
	Find(ctx context.Context, tenantID int64, requestID string) (domain.TurnRecord, error)
	// Acquire moves a queued turn, or a running one redelivered under its current lease, to running
	// with its fence incremented, in the transaction that checks the claim is the current lease of
	// the turn's queue row. A stale or forged claim is domain.ErrFenceLost, a held or suspended turn
	// domain.ErrTurnHeld, and an ended one domain.ErrConflict.
	Acquire(ctx context.Context, claim domain.TurnClaim) (domain.TurnOwner, error)
	// Hold stops the owner's running turn under code, domain.HoldCheckpoint at a durable checkpoint
	// or domain.HoldUnknownOutcome on a call it could not settle: held, fence incremented, a
	// pending hold request cleared, the budget reservation kept. It returns the held fence.
	Hold(ctx context.Context, owner domain.TurnOwner, code domain.TurnHoldCode) (int64, error)
	// Fail marks the owner's running turn failed or cancelled with its error code, in the
	// transaction that writes its settlement.
	Fail(ctx context.Context, owner domain.TurnOwner, status domain.TurnStatus, errorCode string, settlement domain.TurnSettlement) error
	// FailParked fails a turn no worker runs, queued or suspended, with its error code; a held
	// turn is domain.ErrTurnHeld and any other state domain.ErrConflict.
	FailParked(ctx context.Context, tenantID int64, requestID, errorCode string) error
	// Suspend parks the owner's running turn awaiting a human decision with its fence
	// incremented, which it returns. The budget reservation is held, not settled, and the
	// worker releases its lease.
	Suspend(ctx context.Context, owner domain.TurnOwner, reason string) (int64, error)
	// Resume returns a suspended turn to queued with its fence incremented, so a worker replays
	// it from its last checkpoint. A queued turn is a no-op, a held one domain.ErrTurnHeld, and
	// any other domain.ErrConflict.
	Resume(ctx context.Context, tenantID int64, requestID string) error
}

// DataPlane is the composed turn runtime a product depends on: the API process admits and
// cancels turns and reads replies, the worker process leases and executes them.
type DataPlane interface {
	StoredTurnReplyReader
	Runtime() ConversationRuntime
	Scheduler() FairTurnScheduler
	Ingress() ConversationIngress
	Replies() ReplayTurnReplySubscriber
	Canceller() TurnCanceller
	Close() error
}
