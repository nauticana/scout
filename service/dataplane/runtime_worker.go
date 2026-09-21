package dataplane

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelconfig "github.com/nauticana/keel/config"
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
	if scheduler == nil || runtime == nil || workerID == "" || lease <= 0 || batch <= 0 || maxAttempts <= 0 {
		return nil, fmt.Errorf("%w: runtime worker needs a scheduler, a runtime, a worker id, a positive lease, batch, and attempt ceiling", domain.ErrValidation)
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

// QueueTuning is what the queue SQL is built from. A zero field is taken from the
// configured data plane settings, which only exist once keel has loaded them.
type QueueTuning struct {
	// Settings is read once, at the first GetOLTPQueries or QueueQueries.
	Settings    func() domain.DataPlaneSettings
	Lease       time.Duration
	Batch       int
	MaxAttempts int
}

func (tuning QueueTuning) withDefaults(settings domain.DataPlaneSettings) QueueTuning {
	if tuning.Lease <= 0 {
		tuning.Lease = settings.QueueLease
	}
	if tuning.Batch <= 0 {
		tuning.Batch = settings.QueueBatch
	}
	if tuning.MaxAttempts <= 0 {
		tuning.MaxAttempts = settings.QueueMaxAttempts
	}
	return tuning
}

func (tuning QueueTuning) rejectNegative() error {
	if tuning.Lease < 0 || tuning.Batch < 0 || tuning.MaxAttempts < 0 {
		return fmt.Errorf("%w: queue tuning overrides cannot be negative, got %s/%d/%d",
			domain.ErrValidation, tuning.Lease, tuning.Batch, tuning.MaxAttempts)
	}
	return nil
}

func (tuning QueueTuning) validate() error {
	if tuning.Lease <= 0 || tuning.Batch <= 0 || tuning.MaxAttempts <= 0 {
		return fmt.Errorf("%w: queue tuning needs a positive lease, batch, and attempt ceiling, got %s/%d/%d",
			domain.ErrValidation, tuning.Lease, tuning.Batch, tuning.MaxAttempts)
	}
	return nil
}

// ComposingRuntimeWorker defers data-plane composition until keel supplies the
// runtime collaborators to the first claimed job. A failed composition fails that
// job and is retried on the next claim, so a transient failure does not dead-letter the queue.
type ComposingRuntimeWorker struct {
	worker.AbstractWorker
	Compose      DataPlaneComposer
	WorkerID     string
	ExtraQueries map[string]string

	tuning                  QueueTuning
	queries                 map[string]string
	pending, claim, reclaim string
	tuningOnce              sync.Once
	tuningErr               error
	mu                      sync.Mutex
	plane                   contract.DataPlane
	runtimeWorker           *RuntimeWorker
}

// NewComposingRuntimeWorker builds a worker whose queue SQL is built on first use, after
// keel has loaded the configuration the tuning's zero fields come from.
func NewComposingRuntimeWorker(compose DataPlaneComposer, workerID string, tuning QueueTuning) (*ComposingRuntimeWorker, error) {
	if compose == nil || workerID == "" {
		return nil, fmt.Errorf("%w: composing runtime worker needs a composer and a worker id", domain.ErrValidation)
	}
	if err := tuning.rejectNegative(); err != nil {
		return nil, err
	}
	if tuning.Settings == nil {
		if err := tuning.validate(); err != nil {
			return nil, fmt.Errorf("composing runtime worker without a settings source: %w", err)
		}
	}
	return &ComposingRuntimeWorker{Compose: compose, WorkerID: workerID, tuning: tuning}, nil
}

// buildQueries resolves the tuning against the loaded configuration exactly once.
func (w *ComposingRuntimeWorker) buildQueries() error {
	w.tuningOnce.Do(func() {
		tuning := w.tuning
		if tuning.Settings != nil {
			tuning = tuning.withDefaults(tuning.Settings())
		}
		if w.tuningErr = tuning.validate(); w.tuningErr != nil {
			return
		}
		w.tuning = tuning
		w.queries, w.pending, w.claim, w.reclaim = TurnQueueWorkerQueries(w.WorkerID, tuning.Lease, tuning.Batch, tuning.MaxAttempts)
	})
	return w.tuningErr
}

func (w *ComposingRuntimeWorker) GetOLTPQueries() map[string]string {
	if err := w.buildQueries(); err != nil {
		return nil
	}
	queries := maps.Clone(w.queries)
	maps.Copy(queries, w.ExtraQueries)
	return queries
}

func (w *ComposingRuntimeWorker) LeaseClaim() bool { return true }

func (w *ComposingRuntimeWorker) QueueQueries() (pending, claim, reclaim, name string) {
	_ = w.buildQueries()
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
		bounded, hasDeadline := plane.(interface{ LoopDeadline() time.Duration })
		switch {
		case !ok || plane.Runtime() == nil:
			err = fmt.Errorf("%w: composed data plane needs a queue turn scheduler and runtime", domain.ErrNotReady)
		case hasDeadline && w.tuning.Lease <= bounded.LoopDeadline():
			err = fmt.Errorf("%w: runtime worker lease (%s) must outlast the loop deadline (%s)", domain.ErrValidation, w.tuning.Lease, bounded.LoopDeadline())
		default:
			// The worker's resolved ceiling owns delivery. Keep Scout's scheduler on
			// that same ceiling before it can nack the first claimed turn.
			if queued, concrete := scheduler.(*QueueTurnScheduler); concrete {
				queued.MaxAttempts = w.tuning.MaxAttempts
			}
			w.plane = plane
			w.runtimeWorker = &RuntimeWorker{Scheduler: scheduler, Runtime: plane.Runtime(), WorkerID: w.WorkerID}
			return w.runtimeWorker, nil
		}
	}
	if plane != nil {
		err = errors.Join(err, plane.Close())
	}
	return nil, err
}

func (w *ComposingRuntimeWorker) HandleJob(ctx context.Context, journal logger.ApplicationLogger, db port.DatabaseRepository, quota port.QuotaService, qs port.QueryService, jobID int64, row []any) error {
	if err := w.buildQueries(); err != nil {
		return err
	}
	runtimeWorker, err := w.compose(ctx, db)
	if err != nil {
		return fmt.Errorf("compose runtime data plane: %w", err)
	}
	return runtimeWorker.HandleJob(ctx, journal, db, quota, qs, jobID, row)
}

// Run owns the full keel lifecycle and closes the composed plane on shutdown. The queue
// tuning is resolved on the configuration keel has just loaded, so a deployment that
// cannot supply one fails to start instead of leasing turns on a guessed ceiling.
func (w *ComposingRuntimeWorker) Run(ctx context.Context) error {
	w.LoadConfig = w.resolveTuningAfter(w.LoadConfig)
	return errors.Join(w.AbstractWorker.Run(ctx, w), w.Close())
}

func (w *ComposingRuntimeWorker) resolveTuningAfter(loadConfig func(context.Context, port.DatabaseRepository) error) func(context.Context, port.DatabaseRepository) error {
	if loadConfig == nil {
		loadConfig = func(ctx context.Context, db port.DatabaseRepository) error {
			return keelconfig.LoadConfig(ctx, db, *common.NodeId)
		}
	}
	return func(ctx context.Context, db port.DatabaseRepository) error {
		if err := loadConfig(ctx, db); err != nil {
			return err
		}
		return w.buildQueries()
	}
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
