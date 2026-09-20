package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/cache"
	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qCancelRequest = "scout_turn_cancel_request"
	qCancelRead    = "scout_turn_cancel_read"
	qCancelWake    = "scout_turn_cancel_wake"

	cancelChannel           = "scout:cancel"
	defaultCancelPollPeriod = time.Second
	maxCancelReason         = 200
	maxCancelReadFailures   = 3
)

var turnCancelQueries = map[string]string{
	// The first request wins, so the recorded reason is the one that stopped the turn.
	qCancelRequest: `
UPDATE conversation_turn
   SET cancel_requested_at = COALESCE(cancel_requested_at, CURRENT_TIMESTAMP),
       cancel_reason = COALESCE(cancel_reason, ?)
 WHERE tenant_id = ? AND request_id = ? AND status_code IN ('queued', 'running', 'streaming', 'suspended')
RETURNING status_code`,
	// Nothing runs a suspended turn, so it goes back to a worker, which ends it cancelled
	// and releases the reservation the suspension held.
	qCancelWake: `
UPDATE conversation_turn
   SET status_code = 'queued'
 WHERE tenant_id = ? AND request_id = ? AND status_code = 'suspended'
RETURNING turn_no`,
	qQueueRequeue: turnDispatcherQueries[qQueueRequeue],
	qCancelRead: `
SELECT cancel_reason
  FROM conversation_turn
 WHERE tenant_id = ? AND request_id = ? AND cancel_requested_at IS NOT NULL`,
}

// TableTurnCanceller cancels a turn from any process. The request is a flag on
// conversation_turn, so it cannot be lost and also stops a turn that is queued,
// suspended, or picked up later; the worker polls it. Cache is optional and only
// shortens the delay: a lost wake-up costs one poll interval.
type TableTurnCanceller struct {
	DB    keelport.DatabaseRepository
	Cache cache.CacheService
	// PollInterval bounds how long a running turn outlives its cancellation; default 1s.
	PollInterval time.Duration
	Now          func() time.Time

	once   sync.Once
	qs     keelport.QueryService
	signal cacheSignal
}

var (
	_ contract.TurnCanceller     = (*TableTurnCanceller)(nil)
	_ contract.TurnCancelWatcher = (*TableTurnCanceller)(nil)
)

func (canceller *TableTurnCanceller) queries(ctx context.Context) (keelport.QueryService, error) {
	if canceller.DB == nil {
		return nil, fmt.Errorf("turn canceller: database is required")
	}
	canceller.once.Do(func() {
		canceller.qs = canceller.DB.GetQueryService(ctx, turnCancelQueries)
		canceller.signal.cache, canceller.signal.channel = canceller.Cache, cancelChannel
	})
	return canceller.qs, nil
}

func (canceller *TableTurnCanceller) now() time.Time {
	if canceller.Now != nil {
		return canceller.Now()
	}
	return time.Now()
}

func cancelKey(tenantID int64, requestID string) string {
	return fmt.Sprintf("%d:%s", tenantID, requestID)
}

// Close releases the canceller's cache subscription.
func (canceller *TableTurnCanceller) Close() error {
	canceller.signal.close()
	return nil
}

// Cancel records the request; a turn that is unknown or already terminal is domain.ErrNotFound.
func (canceller *TableTurnCanceller) Cancel(ctx context.Context, tenantID int64, requestID, reason string) error {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	if _, err := canceller.queries(ctx); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := canceller.DB.BeginTx(ctx, turnCancelQueries)
	if err != nil {
		return fmt.Errorf("begin turn cancellation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	requested, err := tx.Query(ctx, qCancelRequest, common.TruncateRunes(reason, maxCancelReason), tenantID, requestID)
	if err != nil {
		return fmt.Errorf("request turn cancellation: %w", err)
	}
	if len(requested.Rows) == 0 {
		return fmt.Errorf("%w: no live turn for request %q", domain.ErrNotFound, requestID)
	}
	if common.AsString(requested.Rows[0][0]) == "suspended" {
		if _, err = tx.Query(ctx, qCancelWake, tenantID, requestID); err != nil {
			return fmt.Errorf("wake suspended turn: %w", err)
		}
		if _, err = tx.Query(ctx, qQueueRequeue, canceller.now().UTC(), tenantID, requestID); err != nil {
			return fmt.Errorf("requeue suspended turn: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit turn cancellation: %w", err)
	}
	committed = true
	if canceller.Cache == nil {
		return nil
	}
	if err = canceller.signal.notify(ctx, cancelKey(tenantID, requestID)); err != nil {
		return fmt.Errorf("turn cancellation is recorded, its wake-up failed: %w", err)
	}
	return nil
}

// Watch derives the turn context. A cancellation requested before the turn was
// picked up fails it before any step runs.
func (canceller *TableTurnCanceller) Watch(ctx context.Context, tenantID int64, requestID string) (context.Context, func(), error) {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return nil, nil, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	qs, err := canceller.queries(ctx)
	if err != nil {
		return nil, nil, err
	}
	turnCtx, cancel := context.WithCancelCause(ctx)
	requested := func() (bool, error) {
		result, err := qs.Query(turnCtx, qCancelRead, tenantID, requestID)
		if err != nil || len(result.Rows) == 0 {
			return false, err
		}
		cancel(fmt.Errorf("%w: %s", domain.ErrTurnCanceled, common.AsString(result.Rows[0][0])))
		return true, nil
	}
	if _, err = requested(); err != nil {
		cancel(nil)
		return nil, nil, fmt.Errorf("read turn cancellation: %w", err)
	}
	var wake <-chan struct{}
	unsubscribe := func() {}
	if canceller.Cache != nil {
		if wake, unsubscribe, err = canceller.signal.wait(cancelKey(tenantID, requestID)); err != nil {
			cancel(nil)
			return nil, nil, fmt.Errorf("subscribe to cancellation wake-ups: %w", err)
		}
	}
	period := canceller.PollInterval
	if period <= 0 {
		period = defaultCancelPollPeriod
	}
	go func() {
		poll := time.NewTicker(period)
		defer poll.Stop()
		failures := 0
		for {
			select {
			case <-turnCtx.Done():
				return
			case <-wake:
			case <-poll.C:
			}
			stop, err := requested()
			if failures = failures + 1; err == nil {
				failures = 0
			}
			// A turn whose cancellation cannot be read is not allowed to run unstoppable.
			if failures == maxCancelReadFailures {
				cancel(fmt.Errorf("read turn cancellation: %w", err))
			}
			if stop || failures == maxCancelReadFailures {
				return
			}
		}
	}()
	var once sync.Once
	return turnCtx, func() {
		once.Do(func() {
			unsubscribe()
			cancel(nil)
		})
	}, nil
}
