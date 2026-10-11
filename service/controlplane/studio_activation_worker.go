package controlplane

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nauticana/keel/logger"
	keelport "github.com/nauticana/keel/port"
	"github.com/nauticana/keel/worker"

	"github.com/nauticana/scout/domain"
)

// ActivationComposer builds the activation service once keel has opened the database and
// loaded the configuration.
type ActivationComposer func(ctx context.Context, db keelport.DatabaseRepository) (*ReleaseActivations, error)

// ReleaseActivationWorker is the keel job worker that activates due scheduled publishes.
// It is library code: a product composes it in its own worker binary and calls Run(ctx, w).
type ReleaseActivationWorker struct {
	worker.AbstractWorker
	Compose ActivationComposer
	// Settings is read at every poll; its PollPeriod sets the poll interval once configuration is loaded.
	Settings func() domain.ReleaseActivationSettings

	mu          sync.Mutex
	activations *ReleaseActivations
}

var _ worker.JobWorker = (*ReleaseActivationWorker)(nil)

// NewReleaseActivationWorker wraps loadConfig, which loads keel's and Scout's configuration
// (normally scout.LoadConfig), so the poll interval comes from settings once it is loaded.
func NewReleaseActivationWorker(caption string, compose ActivationComposer, settings func() domain.ReleaseActivationSettings,
	loadConfig func(ctx context.Context, db keelport.DatabaseRepository) error) (*ReleaseActivationWorker, error) {
	if caption == "" || compose == nil || settings == nil || loadConfig == nil {
		return nil, fmt.Errorf("%w: release activation worker needs a caption, a composer, a settings source, and a configuration loader", domain.ErrValidation)
	}
	w := &ReleaseActivationWorker{Compose: compose, Settings: settings}
	w.Caption = caption
	w.LoadConfig = func(ctx context.Context, db keelport.DatabaseRepository) error {
		if err := loadConfig(ctx, db); err != nil {
			return err
		}
		return w.applyPollPeriod()
	}
	return w, nil
}

func (w *ReleaseActivationWorker) applyPollPeriod() error {
	period := w.Settings().PollPeriod
	if period < time.Second {
		return fmt.Errorf("%w: release activation poll period must be at least one second, got %s", domain.ErrValidation, period)
	}
	w.Interval = int(period / time.Second)
	return nil
}

// GetOLTPQueries returns the activation queries; the service also prepares its own.
func (w *ReleaseActivationWorker) GetOLTPQueries() map[string]string { return activationQueries }

// ProcessQueue runs one activation pass and journals its outcome and failures.
func (w *ReleaseActivationWorker) ProcessQueue(ctx context.Context, journal logger.ApplicationLogger, db keelport.DatabaseRepository, _ keelport.QuotaService, _ keelport.QueryService) {
	activations, err := w.compose(ctx, db)
	if err != nil {
		journal.Error(fmt.Sprintf("%s: compose release activations: %v", w.Caption, err))
		return
	}
	report, err := activations.ActivateDue(ctx, w.Settings())
	if report != (domain.ReleaseActivationReport{}) {
		journal.Info(fmt.Sprintf("%s: activated %d, refused %d, deferred %d", w.Caption, report.Activated, report.Refused, report.Deferred))
	}
	if err != nil {
		journal.Error(fmt.Sprintf("%s: %v", w.Caption, err))
	}
}

// compose builds the service once; a failed composition is retried at the next poll.
func (w *ReleaseActivationWorker) compose(ctx context.Context, db keelport.DatabaseRepository) (*ReleaseActivations, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.activations != nil {
		return w.activations, nil
	}
	if w.Compose == nil || w.Settings == nil {
		return nil, fmt.Errorf("%w: release activation worker needs a composer and a settings source", domain.ErrValidation)
	}
	activations, err := w.Compose(ctx, db)
	if err == nil && activations == nil {
		err = fmt.Errorf("%w: activation composer returned nil", domain.ErrNotReady)
	}
	if err != nil {
		return nil, err
	}
	w.activations = activations
	return activations, nil
}
