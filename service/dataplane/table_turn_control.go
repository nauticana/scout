package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nauticana/keel/cache"
	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
)

const (
	qControlCancelRequest = "scout_turn_control_cancel_request"
	qControlRead          = "scout_turn_control_read"
	qControlWake          = "scout_turn_control_wake"
	qControlHold          = "scout_turn_control_hold"
	qControlState         = "scout_turn_control_state"
	qControlRelease       = "scout_turn_control_release"
	qControlPreempt       = "scout_turn_control_preempt"
	qControlUnknown       = "scout_turn_control_unknown_outcomes"

	controlChannel            = "scout:cancel"
	defaultCancelPollPeriod   = time.Second
	maxCancelReason           = 200
	maxHoldReason             = 200
	defaultCancelReadFailures = 3
)

// unheld clears every hold column, the holder's included; a held, operated, or parked turn leaves
// its hold this way.
const unheld = `hold_code = NULL, held_at = NULL, hold_requested_at = NULL, hold_reason = NULL,
       holder_kind = NULL, holder_id = NULL, hold_until = NULL`

// noOpenOutcome gates a transition of the turn aliased turn on none of its calls awaiting an
// outcome. After a hold no fenced writer adds a call and calls only move toward resolution, so
// the gate in the transition's own statement is race-free without a lock.
const noOpenOutcome = `
   AND NOT EXISTS (SELECT 1 FROM turn_call_outcome o
                    WHERE o.partner_id = turn.partner_id AND o.request_id = turn.request_id AND o.status_code IN (` + sqlCallOpen + `))`

var turnControlQueries = map[string]string{
	// The first request wins, so the recorded reason is the one that stopped the turn.
	qControlCancelRequest: `
UPDATE conversation_turn
   SET cancel_requested_at = COALESCE(cancel_requested_at, CURRENT_TIMESTAMP),
       cancel_reason = COALESCE(cancel_reason, ?)
 WHERE partner_id = ? AND request_id = ? AND status_code IN (` + sqlTurnLive + `)
RETURNING status_code`,
	// Nothing runs a suspended, held, or operated turn, so it goes back to a worker under a new
	// fence; the worker ends it cancelled and releases the reservation the turn kept.
	qControlWake: `
UPDATE conversation_turn
   SET status_code = ` + sqlTurnQueued + `, fence = fence + 1, ` + unheld + `
 WHERE partner_id = ? AND request_id = ? AND status_code IN (` + sqlTurnSuspended + `, ` + sqlTurnHeldOrOperated + `)
RETURNING turn_no`,
	qQueueRequeue: turnDispatcherQueries[qQueueRequeue],
	// One poll reads every control signal of the running turn.
	qControlRead: `
SELECT cancel_reason, cancel_requested_at IS NOT NULL, hold_requested_at IS NOT NULL, fence, status_code
  FROM conversation_turn
 WHERE partner_id = ? AND request_id = ?`,
	// A turn no worker runs is held at once under a new fence; a running one is asked to hold,
	// first request wins, and holds itself at its next durable checkpoint.
	qControlHold: `
UPDATE conversation_turn
   SET status_code = CASE WHEN status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `) THEN ` + sqlTurnHeld + ` ELSE status_code END,
       fence = CASE WHEN status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `) THEN fence + 1 ELSE fence END,
       hold_code = CASE WHEN status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `) THEN ? ELSE hold_code END,
       held_at = CASE WHEN status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `) THEN CURRENT_TIMESTAMP ELSE held_at END,
       hold_requested_at = CASE WHEN status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `) THEN NULL
                                ELSE COALESCE(hold_requested_at, CURRENT_TIMESTAMP) END,
       hold_reason = CASE WHEN status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `) THEN NULL ELSE COALESCE(hold_reason, ?) END
 WHERE partner_id = ? AND request_id = ? AND cancel_requested_at IS NULL
   AND status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `, ` + sqlTurnOwned + `)
RETURNING status_code, fence`,
	qControlState: `
SELECT status_code, fence FROM conversation_turn WHERE partner_id = ? AND request_id = ?`,
	qControlRelease: `
UPDATE conversation_turn turn
   SET status_code = ` + sqlTurnQueued + `, fence = fence + 1, ` + unheld + `
 WHERE turn.partner_id = ? AND turn.request_id = ? AND turn.status_code = ` + sqlTurnHeld + ` AND turn.fence = ?` + noOpenOutcome + `
RETURNING turn.fence`,
	// Preemption holds any live turn at once under a new fence; a running worker's next fenced write is refused.
	qControlPreempt: `
UPDATE conversation_turn
   SET status_code = ` + sqlTurnHeld + `, fence = fence + 1, hold_code = ` + sqlHoldPreempted + `,
       held_at = CURRENT_TIMESTAMP, hold_requested_at = NULL, hold_reason = NULL
 WHERE partner_id = ? AND request_id = ? AND cancel_requested_at IS NULL
   AND status_code IN (` + sqlTurnQueued + `, ` + sqlTurnSuspended + `, ` + sqlTurnOwned + `)
RETURNING status_code, fence`,
	// A cancelled turn's in-flight and accepted calls become unknown and stay listed for reconciliation.
	qControlUnknown: `
UPDATE turn_call_outcome SET status_code = ` + sqlCallUnknown + `
 WHERE partner_id = ? AND request_id = ? AND status_code IN (` + sqlCallInFlight + `, ` + sqlCallAccepted + `)
RETURNING call_id`,
}

// TableTurnControl cancels, holds, and releases turns from any process, lets people operate
// held ones, and lets the worker running one watch for those requests. Every request is a column
// of conversation_turn, so it cannot be lost and also reaches a turn that is queued, parked, or
// picked up later; the worker polls it. Cache is optional and only shortens the delay: a lost
// wake-up costs one poll interval.
type TableTurnControl struct {
	DB    keelport.DatabaseRepository
	Cache cache.CacheService
	// PollInterval bounds how long a running turn outlives a request; default 1s.
	PollInterval time.Duration
	// MaxReadFailures consecutive failed control reads stop the turn; default 3.
	MaxReadFailures int
	Now             func() time.Time
	// Objects stores operator entries and the response of a turn a person finishes, Settlement
	// settles that turn, and Operation bounds operation; the operator methods need all three.
	Objects    ObjectStateCodec
	Settlement TurnSettlementWriter
	Operation  domain.OperatorSettings
	// MaxResultCardBytes bounds a finished response copied into turn history; default 64 KiB.
	MaxResultCardBytes int

	once   sync.Once
	qs     keelport.QueryService
	signal cacheSignal
}

var (
	_ contract.TurnCanceller       = (*TableTurnControl)(nil)
	_ contract.TurnControlWatcher  = (*TableTurnControl)(nil)
	_ contract.TurnControl         = (*TableTurnControl)(nil)
	_ contract.OperatorEntryReader = (*TableTurnControl)(nil)
	_ contract.TurnHoldLapser      = (*TableTurnControl)(nil)
)

func (control *TableTurnControl) queries(ctx context.Context) (keelport.QueryService, error) {
	if control.DB == nil {
		return nil, fmt.Errorf("turn control: database is required")
	}
	if control.PollInterval < 0 || control.MaxReadFailures < 0 {
		return nil, fmt.Errorf("turn control: poll interval and read failures must not be negative")
	}
	control.once.Do(func() {
		control.qs = control.DB.GetQueryService(ctx, operationQueries)
		control.signal.cache, control.signal.channel = control.Cache, controlChannel
	})
	return control.qs, nil
}

func (control *TableTurnControl) now() time.Time {
	return clk.Now(control.Now)
}

func controlKey(tenantID int64, requestID string) string {
	return fmt.Sprintf("%d:%s", tenantID, requestID)
}

// Close releases the control's cache subscription.
func (control *TableTurnControl) Close() error {
	control.signal.close()
	return nil
}

// Cancel records the request; a turn that is unknown or already terminal is domain.ErrNotFound.
// A suspended, held, or operated turn is woken under a new fence so a worker ends it.
func (control *TableTurnControl) Cancel(ctx context.Context, tenantID int64, requestID, reason string) error {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	if _, err := control.queries(ctx); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := control.DB.BeginTx(ctx, operationQueries)
	if err != nil {
		return fmt.Errorf("begin turn cancellation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	requested, err := tx.Query(ctx, qControlCancelRequest, common.TruncateRunes(reason, maxCancelReason), tenantID, requestID)
	if err != nil {
		return fmt.Errorf("request turn cancellation: %w", err)
	}
	if len(requested.Rows) == 0 {
		return fmt.Errorf("%w: no live turn for request %q", domain.ErrNotFound, requestID)
	}
	if _, err = tx.Query(ctx, qControlUnknown, tenantID, requestID); err != nil {
		return fmt.Errorf("mark cancelled turn's open calls unknown: %w", err)
	}
	if status := domain.TurnStatus(common.AsString(requested.Rows[0][0])); status == domain.TurnSuspended || status == domain.TurnHeld || status == domain.TurnOperated {
		if _, err = tx.Query(ctx, qControlWake, tenantID, requestID); err != nil {
			return fmt.Errorf("wake parked turn: %w", err)
		}
		if _, err = tx.Query(ctx, qQueueRequeue, control.now().UTC(), tenantID, requestID); err != nil {
			return fmt.Errorf("requeue parked turn: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit turn cancellation: %w", err)
	}
	committed = true
	return control.wake(ctx, tenantID, requestID, "turn cancellation")
}

func (control *TableTurnControl) wake(ctx context.Context, tenantID int64, requestID, what string) error {
	if control.Cache == nil {
		return nil
	}
	if err := control.signal.notify(ctx, controlKey(tenantID, requestID)); err != nil {
		return fmt.Errorf("%s is recorded, its wake-up failed: %w", what, err)
	}
	return nil
}

// RequestHold holds a queued or suspended turn at once and asks a running one to hold at its
// next durable checkpoint.
func (control *TableTurnControl) RequestHold(ctx context.Context, tenantID int64, requestID string, code domain.TurnHoldCode, reason string) (domain.TurnHoldOutcome, error) {
	reason = strings.TrimSpace(reason)
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" || !code.Valid() || reason == "" {
		return domain.TurnHoldOutcome{}, fmt.Errorf("%w: tenant, request, hold code, and reason are required", domain.ErrValidation)
	}
	qs, err := control.queries(ctx)
	if err != nil {
		return domain.TurnHoldOutcome{}, err
	}
	ctx = context.WithoutCancel(ctx)
	held, err := qs.Query(ctx, qControlHold, string(code), common.TruncateRunes(reason, maxHoldReason), tenantID, requestID)
	if err != nil {
		return domain.TurnHoldOutcome{}, fmt.Errorf("hold turn: %w", err)
	}
	if len(held.Rows) > 0 {
		outcome := domain.TurnHoldOutcome{Status: domain.TurnStatus(common.AsString(held.Rows[0][0])), Fence: common.AsInt64(held.Rows[0][1])}
		if outcome.Held() {
			return outcome, nil
		}
		return outcome, control.wake(ctx, tenantID, requestID, "hold request")
	}
	return control.alreadyHeld(ctx, qs, tenantID, requestID)
}

// alreadyHeld reads why a hold matched no turn: a held turn reports its fence, one a person
// operates is domain.ErrTurnHeld, and any other is not a live, uncancelled turn.
func (control *TableTurnControl) alreadyHeld(ctx context.Context, qs keelport.QueryService, tenantID int64, requestID string) (domain.TurnHoldOutcome, error) {
	state, err := qs.Query(ctx, qControlState, tenantID, requestID)
	if err != nil {
		return domain.TurnHoldOutcome{}, fmt.Errorf("read turn state: %w", err)
	}
	if len(state.Rows) > 0 {
		switch status := domain.TurnStatus(common.AsString(state.Rows[0][0])); status {
		case domain.TurnHeld:
			return domain.TurnHoldOutcome{Status: domain.TurnHeld, Fence: common.AsInt64(state.Rows[0][1])}, nil
		case domain.TurnOperated:
			return domain.TurnHoldOutcome{}, fmt.Errorf("%w: turn %q is operated by a person", domain.ErrTurnHeld, requestID)
		}
	}
	return domain.TurnHoldOutcome{}, fmt.Errorf("%w: no live, uncancelled turn for request %q", domain.ErrNotFound, requestID)
}

// Preempt holds a live, uncancelled turn at once under a new fence; a running worker is fenced out
// and woken to notice.
func (control *TableTurnControl) Preempt(ctx context.Context, tenantID int64, requestID string) (domain.TurnHoldOutcome, error) {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return domain.TurnHoldOutcome{}, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	qs, err := control.queries(ctx)
	if err != nil {
		return domain.TurnHoldOutcome{}, err
	}
	ctx = context.WithoutCancel(ctx)
	held, err := qs.Query(ctx, qControlPreempt, tenantID, requestID)
	if err != nil {
		return domain.TurnHoldOutcome{}, fmt.Errorf("preempt turn: %w", err)
	}
	if len(held.Rows) > 0 {
		outcome := domain.TurnHoldOutcome{Status: domain.TurnStatus(common.AsString(held.Rows[0][0])), Fence: common.AsInt64(held.Rows[0][1])}
		return outcome, control.wake(ctx, tenantID, requestID, "preemption")
	}
	return control.alreadyHeld(ctx, qs, tenantID, requestID)
}

// Release returns the turn held at expectedFence to the queue under a new fence, with its queue
// row requeued in the same transaction; a call of the turn whose outcome is open refuses it.
func (control *TableTurnControl) Release(ctx context.Context, tenantID int64, requestID string, expectedFence int64) (int64, error) {
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" || expectedFence <= 0 {
		return 0, fmt.Errorf("%w: tenant, request, and the held fence are required", domain.ErrValidation)
	}
	if _, err := control.queries(ctx); err != nil {
		return 0, err
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := control.DB.BeginTx(ctx, operationQueries)
	if err != nil {
		return 0, fmt.Errorf("begin turn release: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	released, err := tx.Query(ctx, qControlRelease, tenantID, requestID, expectedFence)
	if err != nil {
		return 0, fmt.Errorf("release turn: %w", err)
	}
	if len(released.Rows) == 0 {
		return 0, fmt.Errorf("%w: turn %q is not held at fence %d, or a call of it has an open outcome", domain.ErrConflict, requestID, expectedFence)
	}
	if _, err = tx.Query(ctx, qQueueRequeue, control.now().UTC(), tenantID, requestID); err != nil {
		return 0, fmt.Errorf("requeue released turn: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit turn release: %w", err)
	}
	committed = true
	return common.AsInt64(released.Rows[0][0]), nil
}

// tableTurnWatch is one running turn's view of its control columns.
type tableTurnWatch struct {
	ctx     context.Context
	held    atomic.Bool
	release func()
}

func (watch *tableTurnWatch) Context() context.Context { return watch.ctx }
func (watch *tableTurnWatch) HoldRequested() bool      { return watch.held.Load() }
func (watch *tableTurnWatch) Release()                 { watch.release() }

// Watch derives the owner's turn context. A cancellation requested before the turn was picked
// up fails it before any step runs, and a fence other than the owner's fences it out.
func (control *TableTurnControl) Watch(ctx context.Context, owner domain.TurnOwner) (contract.TurnWatch, error) {
	if err := owner.Check(); err != nil {
		return nil, err
	}
	qs, err := control.queries(ctx)
	if err != nil {
		return nil, err
	}
	turnCtx, cancel := context.WithCancelCause(ctx)
	watch := &tableTurnWatch{ctx: turnCtx}
	// read reports whether the turn must stop; the watch keeps polling for a later cancellation
	// or fence move after a hold request.
	read := func() (bool, error) {
		result, err := qs.Query(turnCtx, qControlRead, owner.TenantID, owner.RequestID)
		if err != nil {
			return false, err
		}
		if len(result.Rows) == 0 {
			cancel(fmt.Errorf("%w: turn %q is gone", domain.ErrFenceLost, owner.RequestID))
			return true, nil
		}
		row := result.Rows[0]
		status := domain.TurnStatus(common.AsString(row[4]))
		switch {
		case common.AsBool(row[1]):
			cancel(fmt.Errorf("%w: %s", domain.ErrTurnCanceled, common.AsString(row[0])))
			return true, nil
		case common.AsInt64(row[3]) != owner.Fence || status != domain.TurnRunning && status != domain.TurnStreaming:
			cancel(fmt.Errorf("%w: turn %q is %s at fence %d", domain.ErrFenceLost, owner.RequestID, status, common.AsInt64(row[3])))
			return true, nil
		}
		if common.AsBool(row[2]) {
			watch.held.Store(true)
		}
		return false, nil
	}
	if _, err = read(); err != nil {
		cancel(nil)
		return nil, fmt.Errorf("read turn control: %w", err)
	}
	var wake <-chan struct{}
	unsubscribe := func() {}
	if control.Cache != nil {
		if wake, unsubscribe, err = control.signal.wait(controlKey(owner.TenantID, owner.RequestID)); err != nil {
			cancel(nil)
			return nil, fmt.Errorf("subscribe to turn control wake-ups: %w", err)
		}
	}
	period, maxFailures := positiveOr(control.PollInterval, defaultCancelPollPeriod), positiveOr(control.MaxReadFailures, defaultCancelReadFailures)
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
			stop, err := read()
			if failures = failures + 1; err == nil {
				failures = 0
			}
			// A turn whose control cannot be read is not allowed to run unstoppable.
			if failures == maxFailures {
				cancel(fmt.Errorf("read turn control: %w", err))
			}
			if stop || failures == maxFailures {
				return
			}
		}
	}()
	var once sync.Once
	watch.release = func() {
		once.Do(func() {
			unsubscribe()
			cancel(nil)
		})
	}
	return watch, nil
}
