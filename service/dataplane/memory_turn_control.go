package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// MemoryTurnControl cancels and holds running turns registered in this process. Workers derive
// their turn context through Watch; Cancel fails it with domain.ErrTurnCanceled and the given
// reason as its cause, and RequestHold sets the hold flag the worker reads at its checkpoints.
// It has no fence authority of its own: the memory stores refuse a fenced-out owner's writes.
type MemoryTurnControl struct {
	mu      sync.Mutex
	watches map[streamKey]*memoryTurnWatch
}

var (
	_ contract.TurnCanceller      = (*MemoryTurnControl)(nil)
	_ contract.TurnControlWatcher = (*MemoryTurnControl)(nil)
)

type memoryTurnWatch struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	held    atomic.Bool
	release func()
}

func (watch *memoryTurnWatch) Context() context.Context { return watch.ctx }
func (watch *memoryTurnWatch) HoldRequested() bool      { return watch.held.Load() }
func (watch *memoryTurnWatch) Release()                 { watch.release() }

// Watch derives a cancelable turn context; the watch must be released.
func (control *MemoryTurnControl) Watch(ctx context.Context, owner domain.TurnOwner) (contract.TurnWatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := owner.Check(); err != nil {
		return nil, err
	}
	key := streamKey{owner.TenantID, owner.RequestID}
	turnCtx, cancel := context.WithCancelCause(ctx)
	watch := &memoryTurnWatch{ctx: turnCtx, cancel: cancel}
	control.mu.Lock()
	if control.watches == nil {
		control.watches = make(map[streamKey]*memoryTurnWatch)
	}
	if _, exists := control.watches[key]; exists {
		control.mu.Unlock()
		cancel(nil)
		return nil, fmt.Errorf("%w: request %q is already watched", domain.ErrConflict, owner.RequestID)
	}
	control.watches[key] = watch
	control.mu.Unlock()

	var releaseOnce sync.Once
	watch.release = func() {
		releaseOnce.Do(func() {
			control.mu.Lock()
			delete(control.watches, key)
			control.mu.Unlock()
			cancel(nil)
		})
	}
	return watch, nil
}

func (control *MemoryTurnControl) watched(ctx context.Context, tenantID int64, requestID string) (*memoryTurnWatch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return nil, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	control.mu.Lock()
	watch := control.watches[streamKey{tenantID, requestID}]
	control.mu.Unlock()
	if watch == nil {
		return nil, fmt.Errorf("%w: no running turn for request %q", domain.ErrNotFound, requestID)
	}
	return watch, nil
}

// Cancel stops the watched turn; an unknown request is domain.ErrNotFound.
func (control *MemoryTurnControl) Cancel(ctx context.Context, tenantID int64, requestID, reason string) error {
	watch, err := control.watched(ctx, tenantID, requestID)
	if err != nil {
		return err
	}
	watch.cancel(fmt.Errorf("%w: %s", domain.ErrTurnCanceled, reason))
	return nil
}

// RequestHold asks the watched turn to hold at its next durable checkpoint; an unknown request
// is domain.ErrNotFound.
func (control *MemoryTurnControl) RequestHold(ctx context.Context, tenantID int64, requestID string) error {
	watch, err := control.watched(ctx, tenantID, requestID)
	if err != nil {
		return err
	}
	watch.held.Store(true)
	return nil
}
