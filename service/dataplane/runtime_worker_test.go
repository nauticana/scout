package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/keel/port"
	"github.com/nauticana/keel/secret"
	"github.com/nauticana/keel/storage"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

type workerRuntimeFunc func(context.Context, domain.TurnDispatch) (domain.TurnResult, error)

func (function workerRuntimeFunc) HandleTurn(ctx context.Context, dispatch domain.TurnDispatch) (domain.TurnResult, error) {
	return function(ctx, dispatch)
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

func TestComposingRuntimeWorkerComposesOnceOnFirstJob(t *testing.T) {
	codec, ref := newSchedulerCodec(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	query := &queueQueryFake{rows: map[string][][]any{qSchedAck: {{int64(11)}}}}
	scheduler := &QueueTurnScheduler{
		DB: queueDBFake{query: query}, Objects: codec, DeadLetters: &fake.DeadLetterQueue{},
		MaxAttempts: 3, PartitionTo: 7, Now: func() time.Time { return now },
	}
	var composeCalls, runtimeCalls int
	plane := &workerPlane{scheduler: scheduler, runtime: workerRuntimeFunc(func(_ context.Context, dispatch domain.TurnDispatch) (domain.TurnResult, error) {
		runtimeCalls++
		if dispatch.Turn.RequestID != "request-1" {
			t.Fatalf("dispatch = %+v", dispatch)
		}
		return domain.TurnResult{}, nil
	})}
	worker, err := NewComposingRuntimeWorker(func(context.Context, port.DatabaseRepository, secret.SecretProvider, storage.ObjectStorage) (contract.DataPlane, error) {
		composeCalls++
		return plane, nil
	}, "runtime-1", time.Minute, 10, 3)
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
	composed := &workerPlane{scheduler: &QueueTurnScheduler{}, runtime: workerRuntimeFunc(nil)}
	composeCalls := 0
	worker, err := NewComposingRuntimeWorker(func(context.Context, port.DatabaseRepository, secret.SecretProvider, storage.ObjectStorage) (contract.DataPlane, error) {
		composeCalls++
		if composeCalls == 1 {
			return failed, boom
		}
		return composed, nil
	}, "runtime-1", time.Minute, 10, 3)
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
