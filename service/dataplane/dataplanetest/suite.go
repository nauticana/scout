// Package dataplanetest holds the turn-lifecycle conformance suites every
// dispatcher, scheduler, idempotency store, and reply broker must pass,
// whether it is a Scout reference adapter or a vendor implementation.
package dataplanetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// QueueHarness is one vendor's durable dispatch path under test. The harness
// owns whatever durable turn identity its dispatcher requires.
type QueueHarness struct {
	Dispatcher contract.TurnDispatcher
	Scheduler  contract.FairTurnScheduler
	// DeadLettered returns the messages parked so far.
	DeadLettered func() []domain.QueueMessage
	// Advance moves the harness clock; nil skips the lease-expiry cases.
	Advance func(time.Duration)
	// MaxAttempts is the scheduler's configured delivery limit.
	MaxAttempts int
	// Lease is the lease duration the suite claims with.
	Lease time.Duration
	// Close releases harness resources; may be nil.
	Close func()
}

// IdempotencyHarness is one vendor's step idempotency store under test.
type IdempotencyHarness struct {
	Store contract.StepIdempotencyStore
	Close func()
}

// ReplyHarness is one vendor's reply broker under test.
type ReplyHarness struct {
	Publisher  contract.TurnReplyPublisher
	Subscriber contract.TurnReplySubscriber
	Close      func()
}

// Dispatch builds a turn dispatch the suites can enqueue.
func Dispatch(tenantID int64, requestID, conversationID string, input []byte) domain.TurnDispatch {
	return domain.TurnDispatch{
		Turn: domain.TurnRequest{
			TenantContext:  domain.TenantContext{TenantID: tenantID},
			RequestID:      requestID,
			ConversationID: conversationID,
			AgentID:        "agent",
			Input:          input,
		},
		ReplyRoute: "route:" + requestID,
	}
}

// RunDispatcherSuite checks durable acceptance: request-id deduplication,
// conflicting input rejection, and per-conversation ordering.
func RunDispatcherSuite(t *testing.T, newHarness func(*testing.T) QueueHarness) {
	t.Helper()
	t.Run("duplicate delivery is a no-op", func(t *testing.T) {
		harness := start(t, newHarness)
		dispatch := Dispatch(1, "request-1", "conversation-1", []byte("input"))
		mustEnqueue(t, harness, dispatch)
		mustEnqueue(t, harness, dispatch)
		lease := mustClaim(t, harness, "worker-1")
		if lease.Message.Dispatch.Turn.RequestID != "request-1" {
			t.Fatalf("claimed %q", lease.Message.Dispatch.Turn.RequestID)
		}
		if err := harness.Scheduler.Ack(context.Background(), lease.Message.MessageID, "worker-1"); err != nil {
			t.Fatal(err)
		}
		if extra, claimed := claim(harness, "worker-1"); claimed {
			t.Fatalf("duplicate enqueue produced a second delivery %q", extra.Message.MessageID)
		}
	})

	t.Run("reused request id with different input conflicts", func(t *testing.T) {
		harness := start(t, newHarness)
		mustEnqueue(t, harness, Dispatch(1, "request-1", "conversation-1", []byte("input")))
		err := harness.Dispatcher.Enqueue(context.Background(), Dispatch(1, "request-1", "conversation-1", []byte("other")))
		if !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("error = %v, want conflict", err)
		}
	})

	t.Run("conversation order is preserved", func(t *testing.T) {
		harness := start(t, newHarness)
		first := Dispatch(1, "request-1", "conversation-1", []byte("first"))
		first.EnqueuedAt = time.Unix(1, 0).UTC()
		second := Dispatch(1, "request-2", "conversation-1", []byte("second"))
		second.EnqueuedAt = time.Unix(2, 0).UTC()
		mustEnqueue(t, harness, first)
		mustEnqueue(t, harness, second)
		lease := mustClaim(t, harness, "worker-1")
		if lease.Message.Dispatch.Turn.RequestID != "request-1" {
			t.Fatalf("claimed %q first", lease.Message.Dispatch.Turn.RequestID)
		}
		if blocked, claimed := claim(harness, "worker-2"); claimed {
			t.Fatalf("second turn of the conversation ran concurrently: %q", blocked.Message.Dispatch.Turn.RequestID)
		}
		if err := harness.Scheduler.Ack(context.Background(), lease.Message.MessageID, "worker-1"); err != nil {
			t.Fatal(err)
		}
		next := mustClaim(t, harness, "worker-2")
		if next.Message.Dispatch.Turn.RequestID != "request-2" {
			t.Fatalf("claimed %q second", next.Message.Dispatch.Turn.RequestID)
		}
	})

	t.Run("separate conversations run concurrently", func(t *testing.T) {
		harness := start(t, newHarness)
		mustEnqueue(t, harness, Dispatch(1, "request-1", "conversation-1", []byte("first")))
		mustEnqueue(t, harness, Dispatch(1, "request-2", "conversation-2", []byte("second")))
		first := mustClaim(t, harness, "worker-1")
		second := mustClaim(t, harness, "worker-2")
		if first.Message.Dispatch.Turn.ConversationID == second.Message.Dispatch.Turn.ConversationID {
			t.Fatalf("claimed the same conversation twice: %q", first.Message.Dispatch.Turn.ConversationID)
		}
	})
}

// RunSchedulerSuite checks lease semantics: fencing, expiry reclaim, retry, and
// dead-lettering on attempt exhaustion.
func RunSchedulerSuite(t *testing.T, newHarness func(*testing.T) QueueHarness) {
	t.Helper()
	t.Run("ack and extend are fenced by worker and lease", func(t *testing.T) {
		harness := start(t, newHarness)
		mustEnqueue(t, harness, Dispatch(1, "request-1", "conversation-1", []byte("input")))
		lease := mustClaim(t, harness, "worker-1")
		ctx := context.Background()
		if err := harness.Scheduler.Ack(ctx, lease.Message.MessageID, "worker-2"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("foreign ack = %v, want conflict", err)
		}
		if err := harness.Scheduler.Extend(ctx, lease.Message.MessageID, "worker-2", harness.leaseDuration()); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("foreign extend = %v, want conflict", err)
		}
		if err := harness.Scheduler.Extend(ctx, lease.Message.MessageID, "worker-1", harness.leaseDuration()); err != nil {
			t.Fatal(err)
		}
		if err := harness.Scheduler.Ack(ctx, lease.Message.MessageID, "worker-1"); err != nil {
			t.Fatal(err)
		}
		if err := harness.Scheduler.Ack(ctx, lease.Message.MessageID, "worker-1"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("repeated ack = %v, want conflict", err)
		}
	})

	t.Run("expired lease is reclaimed and fences the stale worker", func(t *testing.T) {
		harness := start(t, newHarness)
		if harness.Advance == nil {
			t.Skip("harness has no clock control")
		}
		mustEnqueue(t, harness, Dispatch(1, "request-1", "conversation-1", []byte("input")))
		stale := mustClaim(t, harness, "worker-1")
		harness.Advance(harness.leaseDuration() * 2)
		fresh := mustClaim(t, harness, "worker-2")
		if fresh.Message.Dispatch.Turn.RequestID != "request-1" {
			t.Fatalf("reclaimed %q", fresh.Message.Dispatch.Turn.RequestID)
		}
		if err := harness.Scheduler.Ack(context.Background(), stale.Message.MessageID, "worker-1"); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale ack = %v, want conflict", err)
		}
	})

	t.Run("attempt exhaustion dead-letters the delivery", func(t *testing.T) {
		harness := start(t, newHarness)
		if harness.MaxAttempts <= 0 {
			t.Skip("harness reports no attempt limit")
		}
		mustEnqueue(t, harness, Dispatch(1, "request-1", "conversation-1", []byte("input")))
		for attempt := 1; attempt <= harness.MaxAttempts; attempt++ {
			lease, claimed := claim(harness, "worker-1")
			if !claimed {
				t.Fatalf("attempt %d was not deliverable", attempt)
			}
			if err := harness.Scheduler.Nack(context.Background(), lease.Message.MessageID, "worker-1", "boom"); err != nil {
				t.Fatal(err)
			}
			if harness.Advance != nil {
				harness.Advance(time.Hour)
			}
		}
		if _, claimed := claim(harness, "worker-1"); claimed {
			t.Fatal("exhausted delivery is still claimable")
		}
		parked := harness.DeadLettered()
		if len(parked) != 1 || parked[0].Dispatch.Turn.RequestID != "request-1" {
			t.Fatalf("dead letters = %+v", parked)
		}
	})

	t.Run("tenants share the worker pool", func(t *testing.T) {
		harness := start(t, newHarness)
		mustEnqueue(t, harness, Dispatch(1, "tenant1-a", "conversation-1a", []byte("a")))
		mustEnqueue(t, harness, Dispatch(1, "tenant1-b", "conversation-1b", []byte("b")))
		mustEnqueue(t, harness, Dispatch(2, "tenant2-a", "conversation-2a", []byte("a")))
		first := mustClaim(t, harness, "worker-1")
		second := mustClaim(t, harness, "worker-2")
		if first.Message.Dispatch.Turn.TenantContext.TenantID == second.Message.Dispatch.Turn.TenantContext.TenantID {
			t.Fatalf("one tenant took both slots: %d", first.Message.Dispatch.Turn.TenantContext.TenantID)
		}
	})
}

// RunIdempotencySuite checks that an interrupted step replays its stored result
// instead of its side effect.
func RunIdempotencySuite(t *testing.T, newHarness func(*testing.T) IdempotencyHarness) {
	t.Helper()
	step := domain.ExecutionStep{ExecutionStepID: 7, StepID: "step-1", Kind: "deterministic"}
	t.Run("committed step replays its result", func(t *testing.T) {
		harness := newHarness(t)
		if harness.Close != nil {
			t.Cleanup(harness.Close)
		}
		ctx := context.Background()
		if _, replayed, err := harness.Store.Begin(ctx, 1, "request-1", step); err != nil || replayed {
			t.Fatalf("first begin replayed = %v, error = %v", replayed, err)
		}
		want := domain.StepResult{State: []byte("state"), NextStepID: "", Fingerprint: "f", Usage: domain.Usage{OutputTokens: 3, Currency: "USD"}}
		if err := harness.Store.Commit(ctx, 1, "request-1", step, want); err != nil {
			t.Fatal(err)
		}
		got, replayed, err := harness.Store.Begin(ctx, 1, "request-1", step)
		if err != nil || !replayed || string(got.State) != string(want.State) || got.Usage != want.Usage {
			t.Fatalf("replay = %+v, replayed = %v, error = %v", got, replayed, err)
		}
	})

	t.Run("a live claim blocks a second worker", func(t *testing.T) {
		harness := newHarness(t)
		if harness.Close != nil {
			t.Cleanup(harness.Close)
		}
		ctx := context.Background()
		if _, _, err := harness.Store.Begin(ctx, 1, "request-1", step); err != nil {
			t.Fatal(err)
		}
		if _, _, err := harness.Store.Begin(ctx, 1, "request-1", step); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("second begin = %v, want conflict", err)
		}
	})

	t.Run("abandoned step is replayable", func(t *testing.T) {
		harness := newHarness(t)
		if harness.Close != nil {
			t.Cleanup(harness.Close)
		}
		ctx := context.Background()
		if _, _, err := harness.Store.Begin(ctx, 1, "request-1", step); err != nil {
			t.Fatal(err)
		}
		if err := harness.Store.Abandon(ctx, 1, "request-1", step); err != nil {
			t.Fatal(err)
		}
		if _, replayed, err := harness.Store.Begin(ctx, 1, "request-1", step); err != nil || replayed {
			t.Fatalf("reclaim replayed = %v, error = %v", replayed, err)
		}
	})
}

// RunReplySuite checks ordered at-least-once publication with sequence
// deduplication, which is what makes a republished frame safe.
func RunReplySuite(t *testing.T, newHarness func(*testing.T) ReplyHarness) {
	t.Helper()
	frame := func(sequence int64, payload string, final bool) domain.TurnReply {
		return domain.TurnReply{
			TenantID: 1, RequestID: "request-1", ConversationID: "conversation-1",
			ReplyRoute: "route:request-1", Sequence: sequence, Payload: []byte(payload), Final: final,
		}
	}
	t.Run("subscriber receives ordered frames", func(t *testing.T) {
		harness := newHarness(t)
		if harness.Close != nil {
			t.Cleanup(harness.Close)
		}
		ctx := context.Background()
		subscription, err := harness.Subscriber.Subscribe(ctx, 1, "request-1")
		if err != nil {
			t.Fatal(err)
		}
		defer subscription.Close()
		if err := harness.Publisher.Publish(ctx, frame(0, "one", false)); err != nil {
			t.Fatal(err)
		}
		if err := harness.Publisher.Publish(ctx, frame(1, "", true)); err != nil {
			t.Fatal(err)
		}
		first, err := subscription.Receive(ctx)
		if err != nil || string(first.Payload) != "one" {
			t.Fatalf("first frame = %+v, error = %v", first, err)
		}
		last, err := subscription.Receive(ctx)
		if err != nil || !last.Final {
			t.Fatalf("last frame = %+v, error = %v", last, err)
		}
	})

	t.Run("republished frame is a no-op and divergence conflicts", func(t *testing.T) {
		harness := newHarness(t)
		if harness.Close != nil {
			t.Cleanup(harness.Close)
		}
		ctx := context.Background()
		if err := harness.Publisher.Publish(ctx, frame(0, "one", false)); err != nil {
			t.Fatal(err)
		}
		if err := harness.Publisher.Publish(ctx, frame(0, "one", false)); err != nil && !errors.Is(err, domain.ErrReplayExpired) {
			t.Fatalf("republish = %v, want success or expired replay", err)
		}
		err := harness.Publisher.Publish(ctx, frame(0, "different", false))
		if !errors.Is(err, domain.ErrConflict) && !errors.Is(err, domain.ErrReplayExpired) {
			t.Fatalf("divergent republish = %v, want conflict", err)
		}
	})

	t.Run("a skipped sequence is rejected", func(t *testing.T) {
		harness := newHarness(t)
		if harness.Close != nil {
			t.Cleanup(harness.Close)
		}
		ctx := context.Background()
		if err := harness.Publisher.Publish(ctx, frame(0, "one", false)); err != nil {
			t.Fatal(err)
		}
		if err := harness.Publisher.Publish(ctx, frame(2, "three", false)); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("gap = %v, want conflict", err)
		}
	})
}

func (harness QueueHarness) leaseDuration() time.Duration {
	if harness.Lease > 0 {
		return harness.Lease
	}
	return time.Minute
}

func start(t *testing.T, newHarness func(*testing.T) QueueHarness) QueueHarness {
	t.Helper()
	harness := newHarness(t)
	if harness.Dispatcher == nil || harness.Scheduler == nil || harness.DeadLettered == nil {
		t.Fatal("queue harness needs a dispatcher, a scheduler, and a dead-letter view")
	}
	if harness.Close != nil {
		t.Cleanup(harness.Close)
	}
	return harness
}

func mustEnqueue(t *testing.T, harness QueueHarness, dispatch domain.TurnDispatch) {
	t.Helper()
	if err := harness.Dispatcher.Enqueue(context.Background(), dispatch); err != nil {
		t.Fatalf("enqueue %q: %v", dispatch.Turn.RequestID, err)
	}
}

func mustClaim(t *testing.T, harness QueueHarness, workerID string) domain.QueueLease {
	t.Helper()
	lease, claimed := claim(harness, workerID)
	if !claimed {
		t.Fatalf("%s claimed nothing", workerID)
	}
	return lease
}

// claim treats any claim failure as an empty queue: schedulers report "nothing
// ready" with their own sentinel.
func claim(harness QueueHarness, workerID string) (domain.QueueLease, bool) {
	lease, err := harness.Scheduler.Claim(context.Background(), workerID, harness.leaseDuration())
	if err != nil || lease.Message.MessageID == "" {
		return domain.QueueLease{}, false
	}
	return lease, true
}
