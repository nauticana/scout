package dataplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/keel/port"
	"github.com/nauticana/keel/secret"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type workerRuntimeFunc func(context.Context, domain.TurnClaim) (domain.TurnResult, error)

func (function workerRuntimeFunc) HandleTurn(ctx context.Context, claim domain.TurnClaim) (domain.TurnResult, error) {
	return function(ctx, claim)
}

type workerPlane struct {
	scheduler contract.FairTurnScheduler
	runtime   contract.ConversationRuntime
	closed    int
}

func (plane *workerPlane) Runtime() contract.ConversationRuntime { return plane.runtime }
func (plane *workerPlane) Scheduler() contract.FairTurnScheduler { return plane.scheduler }
func (*workerPlane) Ingress() contract.ConversationIngress       { return nil }
func (*workerPlane) Replies() contract.ReplayTurnReplySubscriber { return nil }
func (*workerPlane) Canceller() contract.TurnCanceller           { return nil }
func (*workerPlane) StoredReply(context.Context, int64, string, int64) (domain.TurnReply, error) {
	return domain.TurnReply{}, domain.ErrNotFound
}
func (plane *workerPlane) Close() error { plane.closed++; return nil }

type deadlinePlane struct{ *workerPlane }

func (deadlinePlane) LoopDeadline() time.Duration { return 5 * time.Minute }

func TestComposingRuntimeWorkerComposesOnceOnFirstJob(t *testing.T) {
	codec, ref := newSchedulerCodec(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	query := &queueQueryFake{rows: map[string][][]any{qSchedAck: {{int64(11)}}}}
	scheduler := &QueueTurnScheduler{
		DB: queueDBFake{query: query}, Objects: codec, DeadLetters: &fake.DeadLetterQueue{},
		MaxAttempts: 3, PartitionTo: 7, Now: func() time.Time { return now },
	}
	var composeCalls, runtimeCalls int
	plane := &workerPlane{scheduler: scheduler, runtime: workerRuntimeFunc(func(_ context.Context, claim domain.TurnClaim) (domain.TurnResult, error) {
		runtimeCalls++
		// The leased row is the claim: its queue id, its lease token, and this worker.
		if claim.Dispatch.Turn.RequestID != "request-1" || claim.QueueID != 11 || claim.LeaseToken != 99 || claim.WorkerID != "runtime-1" {
			t.Fatalf("claim = %+v", claim)
		}
		return domain.TurnResult{}, nil
	})}
	worker, err := NewComposingRuntimeWorker(func(context.Context, port.DatabaseRepository, secret.SecretProvider) (contract.DataPlane, error) {
		composeCalls++
		return plane, nil
	}, "runtime-1", QueueTuning{Lease: time.Minute, Batch: 10, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	row := queueRow(11, 7, "request-1", "conversation-1", ref, 1, 99, now, now.Add(time.Minute))
	if err = worker.HandleJob(context.Background(), nil, queueDBFake{query: query}, nil, nil, 11, row); err != nil {
		t.Fatal(err)
	}
	query.rows[qSchedAck] = [][]any{{int64(11)}}
	if err = worker.HandleJob(context.Background(), nil, queueDBFake{query: query}, nil, nil, 11, row); err != nil {
		t.Fatal(err)
	}
	if composeCalls != 1 || runtimeCalls != 2 {
		t.Fatalf("compose calls = %d, runtime calls = %d", composeCalls, runtimeCalls)
	}
	if err = worker.Close(); err != nil || plane.closed != 1 {
		t.Fatalf("close error = %v, calls = %d", err, plane.closed)
	}
}

func TestComposingRuntimeWorkerRetriesAFailedCompositionAndClosesItsPlane(t *testing.T) {
	boom := errors.New("compose failed")
	failed := &workerPlane{}
	composed := &workerPlane{scheduler: &QueueTurnScheduler{MaxAttempts: 3}, runtime: workerRuntimeFunc(nil)}
	composeCalls := 0
	worker, err := NewComposingRuntimeWorker(func(context.Context, port.DatabaseRepository, secret.SecretProvider) (contract.DataPlane, error) {
		composeCalls++
		if composeCalls == 1 {
			return failed, boom
		}
		return composed, nil
	}, "runtime-1", QueueTuning{Lease: time.Minute, Batch: 10, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if handleErr := worker.HandleJob(context.Background(), nil, nil, nil, nil, 1, nil); !errors.Is(handleErr, boom) || failed.closed != 1 {
		t.Fatalf("HandleJob error = %v, failed plane closed = %d", handleErr, failed.closed)
	}
	if _, err = worker.compose(context.Background(), nil); err != nil || composeCalls != 2 {
		t.Fatalf("recomposition error = %v, compose calls = %d", err, composeCalls)
	}
	if err = errors.Join(worker.Close(), worker.Close()); err != nil || composed.closed != 1 {
		t.Fatalf("close error = %v, calls = %d", err, composed.closed)
	}
}

func TestComposingRuntimeWorkerTakesItsQueueTuningFromTheLoadedConfiguration(t *testing.T) {
	settings := domain.DataPlaneSettings{QueueLease: 15 * time.Minute, QueueBatch: 8, QueueMaxAttempts: 5}
	reads := 0
	worker, err := NewComposingRuntimeWorker(func(context.Context, port.DatabaseRepository, secret.SecretProvider) (contract.DataPlane, error) {
		return nil, nil
	}, "runtime-1", QueueTuning{Settings: func() domain.DataPlaneSettings {
		reads++
		return settings
	}, Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("the settings must not be read before keel loads them, got %d reads", reads)
	}
	loaded := false
	loadConfig := worker.resolveTuningAfter(func(context.Context, port.DatabaseRepository) error {
		loaded = true
		return nil
	})
	if err = loadConfig(context.Background(), nil); err != nil || !loaded {
		t.Fatalf("load config = %v, loaded = %t", err, loaded)
	}
	queries := worker.GetOLTPQueries()
	pending, claim, _, _ := worker.QueueQueries()
	if reads != 1 {
		t.Fatalf("settings reads = %d, want one after the configuration is loaded", reads)
	}
	if !strings.Contains(queries[claim], "INTERVAL '900 seconds'") || !strings.Contains(queries[pending], "attempt < 5") {
		t.Fatalf("queue SQL must follow the configuration:\n%s\n%s", queries[claim], queries[pending])
	}
	if !strings.Contains(queries[pending], "LIMIT 2") {
		t.Fatalf("an explicit batch must override the configuration: %s", queries[pending])
	}
}

func TestComposingRuntimeWorkerRefusesTuningItCannotResolve(t *testing.T) {
	compose := func(context.Context, port.DatabaseRepository, secret.SecretProvider) (contract.DataPlane, error) {
		return nil, nil
	}
	if _, err := NewRuntimeWorker(&QueueTurnScheduler{}, workerRuntimeFunc(nil), "runtime-1", time.Minute, 0, 3); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a runtime worker must reject a non-positive batch, got %v", err)
	}
	if _, err := NewComposingRuntimeWorker(compose, "runtime-1", QueueTuning{Lease: time.Minute}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a tuning without a settings source must be complete, got %v", err)
	}
	if _, err := NewComposingRuntimeWorker(compose, "runtime-1", QueueTuning{
		Settings: func() domain.DataPlaneSettings {
			return domain.DataPlaneSettings{QueueLease: time.Minute, QueueBatch: 4, QueueMaxAttempts: 3}
		}, Batch: -1,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative override must not fall back to the settings, got %v", err)
	}
	worker, err := NewComposingRuntimeWorker(compose, "runtime-1", QueueTuning{Settings: func() domain.DataPlaneSettings {
		return domain.DataPlaneSettings{}
	}})
	if err != nil {
		t.Fatal(err)
	}
	loaded := false
	loadConfig := worker.resolveTuningAfter(func(context.Context, port.DatabaseRepository) error {
		loaded = true
		return nil
	})
	if err = loadConfig(context.Background(), nil); !errors.Is(err, domain.ErrValidation) || !loaded {
		t.Fatalf("a worker that cannot resolve its tuning must fail to start, got %v", err)
	}
	if queries := worker.GetOLTPQueries(); queries != nil {
		t.Fatalf("unresolved tuning must publish no queue SQL, got %v", queries)
	}
	if err = worker.HandleJob(context.Background(), nil, nil, nil, nil, 1, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("HandleJob = %v, want the tuning failure", err)
	}
}

func TestComposingRuntimeWorkerAlignsScoutSchedulerAttemptCeiling(t *testing.T) {
	plane := &workerPlane{scheduler: &QueueTurnScheduler{MaxAttempts: 9}, runtime: workerRuntimeFunc(nil)}
	worker, err := NewComposingRuntimeWorker(func(context.Context, port.DatabaseRepository, secret.SecretProvider) (contract.DataPlane, error) {
		return plane, nil
	}, "runtime-1", QueueTuning{Lease: time.Minute, Batch: 4, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.buildQueries(); err != nil {
		t.Fatal(err)
	}
	if _, err = worker.compose(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if plane.scheduler.(*QueueTurnScheduler).MaxAttempts != 3 {
		t.Fatalf("scheduler attempts = %d, want the queue SQL ceiling 3", plane.scheduler.(*QueueTurnScheduler).MaxAttempts)
	}
}

func TestComposingRuntimeWorkerRefusesALeaseUnderThePlaneLoopDeadline(t *testing.T) {
	plane := &workerPlane{scheduler: &QueueTurnScheduler{MaxAttempts: 3}, runtime: workerRuntimeFunc(nil)}
	worker, err := NewComposingRuntimeWorker(func(context.Context, port.DatabaseRepository, secret.SecretProvider) (contract.DataPlane, error) {
		return deadlinePlane{plane}, nil
	}, "runtime-1", QueueTuning{Lease: time.Minute, Batch: 4, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	err = worker.HandleJob(context.Background(), nil, nil, nil, nil, 1, nil)
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "loop deadline") || plane.closed != 1 {
		t.Fatalf("HandleJob = %v, plane closed = %d", err, plane.closed)
	}
}

// A delivery that no longer owns its turn, or finds it held, is acknowledged while its lease
// stands and dropped once it is gone; it is never nacked into a retry or a dead letter.
func TestRuntimeWorkerAcknowledgesOrDropsAStaleOrHeldDelivery(t *testing.T) {
	codec, ref := newSchedulerCodec(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	row := queueRow(11, 7, "request-1", "conversation-1", ref, 1, 99, now, now.Add(time.Minute))
	for name, tc := range map[string]struct {
		outcome error
		acked   [][]any
	}{
		"held, lease current":       {outcome: domain.ErrTurnHeld, acked: [][]any{{int64(11)}}},
		"held, lease gone":          {outcome: domain.ErrTurnHeld},
		"fenced out, lease current": {outcome: domain.ErrFenceLost, acked: [][]any{{int64(11)}}},
		"fenced out, lease gone":    {outcome: domain.ErrFenceLost},
	} {
		query := &queueQueryFake{rows: map[string][][]any{qSchedAck: tc.acked}}
		scheduler := &QueueTurnScheduler{
			DB: queueDBFake{query: query}, Objects: codec, DeadLetters: &fake.DeadLetterQueue{},
			MaxAttempts: 3, PartitionTo: 7, Now: func() time.Time { return now },
		}
		worker, err := NewRuntimeWorker(scheduler, workerRuntimeFunc(func(context.Context, domain.TurnClaim) (domain.TurnResult, error) {
			return domain.TurnResult{}, fmt.Errorf("%w: turn request-1", tc.outcome)
		}), "runtime-1", time.Minute, 1, 3)
		if err != nil {
			t.Fatal(err)
		}
		if err = worker.HandleJob(context.Background(), nil, nil, nil, nil, 11, row); err != nil {
			t.Fatalf("%s: HandleJob = %v", name, err)
		}
		if !slices.Contains(query.calls, qSchedAck) || slices.Contains(query.calls, qSchedLockLeased) || slices.Contains(query.calls, qSchedRetry) {
			t.Fatalf("%s: calls = %v, want an ack and no nack", name, query.calls)
		}
	}
}
