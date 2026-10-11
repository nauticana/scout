package dataplane

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nauticana/keel/logger"
	keelport "github.com/nauticana/keel/port"
	"github.com/nauticana/keel/worker"

	"github.com/nauticana/scout/domain"
)

// HealthComposer builds the health service once keel has opened the database and loaded the configuration.
type HealthComposer func(ctx context.Context, db keelport.DatabaseRepository) (*AgentHealth, error)

// AgentHealthWorker is the keel job worker that evaluates agents' health alerts every period.
// It is library code: a product composes it in its own worker binary and calls Run(ctx, w).
type AgentHealthWorker struct {
	worker.AbstractWorker
	Compose HealthComposer
	// Settings is read once configuration is loaded; its Period sets the interval.
	Settings func() domain.AgentHealthSettings

	mu     sync.Mutex
	health *AgentHealth
}

var _ worker.JobWorker = (*AgentHealthWorker)(nil)

// NewAgentHealthWorker wraps loadConfig, which loads keel's and Scout's configuration (normally
// scout.LoadConfig), so the interval comes from settings once it is loaded.
func NewAgentHealthWorker(caption string, compose HealthComposer, settings func() domain.AgentHealthSettings,
	loadConfig func(ctx context.Context, db keelport.DatabaseRepository) error) (*AgentHealthWorker, error) {
	if caption == "" || compose == nil || settings == nil || loadConfig == nil {
		return nil, fmt.Errorf("%w: agent health worker needs a caption, a composer, a settings source, and a configuration loader", domain.ErrValidation)
	}
	w := &AgentHealthWorker{Compose: compose, Settings: settings}
	w.Caption = caption
	w.LoadConfig = func(ctx context.Context, db keelport.DatabaseRepository) error {
		if err := loadConfig(ctx, db); err != nil {
			return err
		}
		return w.applyPeriod()
	}
	return w, nil
}

func (w *AgentHealthWorker) applyPeriod() error {
	period := cmp.Or(w.Settings().Period, DefaultHealthPeriod)
	if period < time.Second {
		return fmt.Errorf("%w: agent health period must be at least one second, got %s", domain.ErrValidation, period)
	}
	w.Interval = int(period / time.Second)
	return nil
}

// GetOLTPQueries returns the health queries; the service also prepares its own.
func (w *AgentHealthWorker) GetOLTPQueries() map[string]string { return healthQueries }

// ProcessQueue runs one evaluation pass and journals its counts and failures, undelivered alerts among them.
func (w *AgentHealthWorker) ProcessQueue(ctx context.Context, journal logger.ApplicationLogger, db keelport.DatabaseRepository, _ keelport.QuotaService, _ keelport.QueryService) {
	health, err := w.compose(ctx, db)
	if err != nil {
		journal.Error(fmt.Sprintf("%s: compose agent health: %v", w.Caption, err))
		return
	}
	report, err := health.EvaluateAll(ctx)
	if report != (domain.HealthEvaluationReport{}) {
		journal.Info(fmt.Sprintf("%s: evaluated %d, skipped %d, opened %d, resolved %d, undelivered %d, failed %d", w.Caption,
			report.Evaluated, report.Skipped, report.Opened, report.Resolved, report.Undelivered, report.Failed))
	}
	if err != nil {
		journal.Error(fmt.Sprintf("%s: %v", w.Caption, err))
	}
}

// compose builds the service once; a failed composition is retried at the next pass.
func (w *AgentHealthWorker) compose(ctx context.Context, db keelport.DatabaseRepository) (*AgentHealth, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.health != nil {
		return w.health, nil
	}
	if w.Compose == nil {
		return nil, fmt.Errorf("%w: agent health worker needs a composer", domain.ErrValidation)
	}
	health, err := w.Compose(ctx, db)
	if err == nil && health == nil {
		err = fmt.Errorf("%w: agent health composer returned nil", domain.ErrNotReady)
	}
	if err != nil {
		return nil, err
	}
	w.health = health
	return health, nil
}
