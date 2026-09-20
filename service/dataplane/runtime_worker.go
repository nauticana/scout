package dataplane

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/nauticana/keel/logger"
	"github.com/nauticana/keel/port"
	"github.com/nauticana/keel/worker"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// RuntimeWorker is the keel leased queue worker that drains turn_queue into a
// ConversationRuntime. A turn that returns an error is nacked for bounded retry;
// a turn the runtime suspended or finished is acked.
type RuntimeWorker struct {
	worker.AbstractWorker
	Scheduler *QueueTurnScheduler
	Runtime   contract.ConversationRuntime
	WorkerID  string
	// ExtraQueries joins the worker's named SQL, for services sharing its query service.
	ExtraQueries map[string]string

	queries                 map[string]string
	pending, claim, reclaim string
}

// NewRuntimeWorker validates the composition and builds the queue's named SQL.
func NewRuntimeWorker(scheduler *QueueTurnScheduler, runtime contract.ConversationRuntime, workerID string, lease time.Duration, batch, maxAttempts int) (*RuntimeWorker, error) {
	if scheduler == nil || runtime == nil || workerID == "" || lease <= 0 {
		return nil, fmt.Errorf("%w: runtime worker needs a scheduler, a runtime, a worker id, and a positive lease", domain.ErrValidation)
	}
	w := &RuntimeWorker{Scheduler: scheduler, Runtime: runtime, WorkerID: workerID}
	w.queries, w.pending, w.claim, w.reclaim = TurnQueueWorkerQueries(workerID, lease, batch, maxAttempts)
	return w, nil
}

func (w *RuntimeWorker) GetOLTPQueries() map[string]string {
	queries := maps.Clone(w.queries)
	maps.Copy(queries, w.ExtraQueries)
	return queries
}

func (w *RuntimeWorker) LeaseClaim() bool { return true }

func (w *RuntimeWorker) QueueQueries() (pending, claim, reclaim, name string) {
	return w.pending, w.claim, w.reclaim, "runtime"
}

func (w *RuntimeWorker) HandleJob(ctx context.Context, _ logger.ApplicationLogger, _ port.DatabaseRepository, _ port.QuotaService, _ port.QueryService, _ int64, row []any) error {
	lease, err := w.Scheduler.LeaseFromClaimRow(ctx, row)
	if err != nil {
		return err
	}
	if _, err = w.Runtime.HandleTurn(ctx, lease.Message.Dispatch); err != nil {
		return w.Scheduler.Nack(context.WithoutCancel(ctx), lease.Message.MessageID, w.WorkerID, err.Error())
	}
	return w.Scheduler.Ack(context.WithoutCancel(ctx), lease.Message.MessageID, w.WorkerID)
}

var _ worker.LeasedQueueWorker = (*RuntimeWorker)(nil)
