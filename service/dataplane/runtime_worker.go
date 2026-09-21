package dataplane

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/nauticana/keel/logger"
	"github.com/nauticana/keel/port"
	"github.com/nauticana/keel/secret"
	"github.com/nauticana/keel/storage"
	"github.com/nauticana/keel/worker"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// RuntimeWorker is the keel leased queue worker that drains turn_queue into a
// ConversationRuntime. A turn that returns an error is nacked for bounded retry;
// a turn the runtime suspended or finished is acked.
type RuntimeWorker struct {
	worker.AbstractWorker
	Scheduler contract.ClaimedTurnScheduler
	Runtime   contract.ConversationRuntime
	WorkerID  string
	// ExtraQueries joins the worker's named SQL, for services sharing its query service.
	ExtraQueries map[string]string

	queries                 map[string]string
	pending, claim, reclaim string
}

// NewRuntimeWorker validates the composition and builds the queue's named SQL.
func NewRuntimeWorker(scheduler contract.ClaimedTurnScheduler, runtime contract.ConversationRuntime, workerID string, lease time.Duration, batch, maxAttempts int) (*RuntimeWorker, error) {
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

// DataPlaneComposer builds a data plane after keel has initialized the process
// database, secret provider, configuration, and object storage.
type DataPlaneComposer func(context.Context, port.DatabaseRepository, secret.SecretProvider, storage.ObjectStorage) (contract.DataPlane, error)

// ComposingRuntimeWorker defers data-plane composition until keel supplies the
// runtime collaborators to the first claimed job. A failed composition fails that
// job and is retried on the next claim, so a transient failure does not dead-letter the queue.
type ComposingRuntimeWorker struct {
	worker.AbstractWorker
	Compose      DataPlaneComposer
	WorkerID     string
	ExtraQueries map[string]string

	queries                 map[string]string
	pending, claim, reclaim string
	mu                      sync.Mutex
	plane                   contract.DataPlane
	runtimeWorker           *RuntimeWorker
}

// NewComposingRuntimeWorker builds a worker whose queue SQL is available before
// the data plane and database are composed.
func NewComposingRuntimeWorker(compose DataPlaneComposer, workerID string, lease time.Duration, batch, maxAttempts int) (*ComposingRuntimeWorker, error) {
	if compose == nil || workerID == "" || lease <= 0 || batch <= 0 || maxAttempts <= 0 {
		return nil, fmt.Errorf("%w: composing runtime worker needs a composer, worker id, positive lease, batch, and attempts", domain.ErrValidation)
	}
	w := &ComposingRuntimeWorker{Compose: compose, WorkerID: workerID}
	w.queries, w.pending, w.claim, w.reclaim = TurnQueueWorkerQueries(workerID, lease, batch, maxAttempts)
	return w, nil
}

func (w *ComposingRuntimeWorker) GetOLTPQueries() map[string]string {
	queries := maps.Clone(w.queries)
	maps.Copy(queries, w.ExtraQueries)
	return queries
}

func (w *ComposingRuntimeWorker) LeaseClaim() bool { return true }

func (w *ComposingRuntimeWorker) QueueQueries() (pending, claim, reclaim, name string) {
	return w.pending, w.claim, w.reclaim, "runtime"
}

func (w *ComposingRuntimeWorker) compose(ctx context.Context, db port.DatabaseRepository) (*RuntimeWorker, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.runtimeWorker != nil {
		return w.runtimeWorker, nil
	}
	plane, err := w.Compose(ctx, db, w.Secret, w.Storage)
	if err == nil && plane == nil {
		err = fmt.Errorf("%w: data plane composer returned nil", domain.ErrNotReady)
	}
	if err == nil {
		scheduler, ok := plane.Scheduler().(contract.ClaimedTurnScheduler)
		if ok && plane.Runtime() != nil {
			w.plane = plane
			w.runtimeWorker = &RuntimeWorker{Scheduler: scheduler, Runtime: plane.Runtime(), WorkerID: w.WorkerID}
			return w.runtimeWorker, nil
		}
		err = fmt.Errorf("%w: composed data plane needs a queue turn scheduler and runtime", domain.ErrNotReady)
	}
	if plane != nil {
		err = errors.Join(err, plane.Close())
	}
	return nil, err
}

func (w *ComposingRuntimeWorker) HandleJob(ctx context.Context, journal logger.ApplicationLogger, db port.DatabaseRepository, quota port.QuotaService, qs port.QueryService, jobID int64, row []any) error {
	runtimeWorker, err := w.compose(ctx, db)
	if err != nil {
		return fmt.Errorf("compose runtime data plane: %w", err)
	}
	return runtimeWorker.HandleJob(ctx, journal, db, quota, qs, jobID, row)
}

// Run owns the full keel lifecycle and closes the composed plane on shutdown.
func (w *ComposingRuntimeWorker) Run(ctx context.Context) error {
	return errors.Join(w.AbstractWorker.Run(ctx, w), w.Close())
}

// Close releases the composed plane; a later job composes a new one.
func (w *ComposingRuntimeWorker) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.plane == nil {
		return nil
	}
	plane := w.plane
	w.plane, w.runtimeWorker = nil, nil
	return plane.Close()
}

var _ worker.LeasedQueueWorker = (*ComposingRuntimeWorker)(nil)
