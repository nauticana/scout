package dataplane

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nauticana/keel/logger"
	keelport "github.com/nauticana/keel/port"
	"github.com/nauticana/keel/worker"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
	"github.com/nauticana/scout/service/observability"
)

// LapseComposer builds the hold lapser and the audit sink once keel has opened the database and
// loaded the configuration; a nil sink audits nothing.
type LapseComposer func(ctx context.Context, db keelport.DatabaseRepository) (contract.TurnHoldLapser, contract.AuditSink, error)

// TurnHoldLapseWorker is the keel job worker that, every period, holds again the operated turns
// whose lease lapsed by the store clock: operated to held with the fence incremented, in batches
// of the configured size until none is left, each audited turn_hold_lapsed. It is library code:
// a product composes it in its own worker binary and calls Run(ctx, w).
type TurnHoldLapseWorker struct {
	worker.AbstractWorker
	Compose LapseComposer
	// Settings is read once configuration is loaded; its LapsePeriod sets the interval and its
	// LapseBatch bounds one statement.
	Settings func() domain.OperatorSettings
	Now      func() time.Time

	mu     sync.Mutex
	lapser contract.TurnHoldLapser
	audit  contract.AuditSink
}

var _ worker.JobWorker = (*TurnHoldLapseWorker)(nil)

// NewTurnHoldLapseWorker wraps loadConfig, which loads keel's and Scout's configuration (normally
// scout.LoadConfig), so the interval comes from the settings once they are loaded.
func NewTurnHoldLapseWorker(caption string, compose LapseComposer, settings func() domain.OperatorSettings,
	loadConfig func(ctx context.Context, db keelport.DatabaseRepository) error) (*TurnHoldLapseWorker, error) {
	if caption == "" || compose == nil || settings == nil || loadConfig == nil {
		return nil, fmt.Errorf("%w: hold lapse worker needs a caption, a composer, a settings source, and a configuration loader", domain.ErrValidation)
	}
	w := &TurnHoldLapseWorker{Compose: compose, Settings: settings}
	w.Caption = caption
	w.LoadConfig = func(ctx context.Context, db keelport.DatabaseRepository) error {
		if err := loadConfig(ctx, db); err != nil {
			return err
		}
		return w.applyPeriod()
	}
	return w, nil
}

func (w *TurnHoldLapseWorker) applyPeriod() error {
	period := cmp.Or(w.Settings().LapsePeriod, DefaultHoldLapsePeriod)
	if period < time.Second || period > MaxHoldLapsePeriod {
		return fmt.Errorf("%w: hold lapse period must lie within 1s..%s, got %s", domain.ErrValidation, MaxHoldLapsePeriod, period)
	}
	w.Interval = int(period / time.Second)
	return nil
}

// GetOLTPQueries returns the turn control statements the lapse runs.
func (w *TurnHoldLapseWorker) GetOLTPQueries() map[string]string { return operationQueries }

// ProcessQueue lapses every due hold, batch by batch, and journals the count and any failure.
func (w *TurnHoldLapseWorker) ProcessQueue(ctx context.Context, journal logger.ApplicationLogger, db keelport.DatabaseRepository, _ keelport.QuotaService, _ keelport.QueryService) {
	lapsed, err := w.Sweep(ctx, db)
	if lapsed > 0 {
		journal.Info(fmt.Sprintf("%s: held again %d turns whose operator lease lapsed", w.Caption, lapsed))
	}
	if err != nil {
		journal.Error(fmt.Sprintf("%s: %v", w.Caption, err))
	}
}

// Sweep runs one pass: a batch at a time until a batch comes back short, so a pass ends once no
// due hold is left. Each lapsed hold is audited after its batch commits.
func (w *TurnHoldLapseWorker) Sweep(ctx context.Context, db keelport.DatabaseRepository) (int, error) {
	lapser, audit, err := w.compose(ctx, db)
	if err != nil {
		return 0, err
	}
	batch := cmp.Or(w.Settings().LapseBatch, DefaultHoldLapseBatch)
	if batch < 1 || batch > MaxHoldLapseBatch {
		return 0, fmt.Errorf("%w: hold lapse batch must lie within 1..%d, got %d", domain.ErrValidation, MaxHoldLapseBatch, batch)
	}
	total := 0
	for {
		holds, err := lapser.LapseHolds(ctx, batch)
		total += len(holds)
		var failures []error
		for _, hold := range holds {
			actor := domain.StudioActor{TenantID: hold.TenantID, Principal: hold.Holder}
			if auditErr := auditTurnControl(ctx, audit, actor, hold.AgentID, hold.RequestID, observability.AuditCategoryTurnHoldLapsed, hold.Fence, "lease lapsed", w.now()); auditErr != nil {
				failures = append(failures, auditErr)
			}
		}
		if err != nil || len(failures) > 0 {
			return total, errors.Join(append(failures, err)...)
		}
		if len(holds) < batch {
			return total, nil
		}
	}
}

func (w *TurnHoldLapseWorker) now() time.Time {
	return clk.Now(w.Now)
}

// compose builds the lapser once; a failed composition is retried at the next pass.
func (w *TurnHoldLapseWorker) compose(ctx context.Context, db keelport.DatabaseRepository) (contract.TurnHoldLapser, contract.AuditSink, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lapser != nil {
		return w.lapser, w.audit, nil
	}
	if w.Compose == nil {
		return nil, nil, fmt.Errorf("%w: hold lapse worker needs a composer", domain.ErrValidation)
	}
	lapser, audit, err := w.Compose(ctx, db)
	if err == nil && lapser == nil {
		err = fmt.Errorf("%w: hold lapse composer returned no lapser", domain.ErrNotReady)
	}
	if err != nil {
		return nil, nil, err
	}
	w.lapser, w.audit = lapser, audit
	return lapser, audit, nil
}
