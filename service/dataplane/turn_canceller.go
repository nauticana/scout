package dataplane

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// MemoryTurnCanceller cancels running turns registered in this process.
// Workers derive their turn context through Watch; Cancel fails the derived
// context with domain.ErrTurnCanceled and the given reason as its cause.
type MemoryTurnCanceller struct {
	mu      sync.Mutex
	watches map[streamKey]context.CancelCauseFunc
}

var _ contract.TurnCanceller = (*MemoryTurnCanceller)(nil)

// Watch derives a cancelable turn context; the release func must be deferred.
func (canceller *MemoryTurnCanceller) Watch(ctx context.Context, tenantID int64, requestID string) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return nil, nil, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	key := streamKey{tenantID, requestID}
	turnCtx, cancel := context.WithCancelCause(ctx)
	canceller.mu.Lock()
	if canceller.watches == nil {
		canceller.watches = make(map[streamKey]context.CancelCauseFunc)
	}
	if _, exists := canceller.watches[key]; exists {
		canceller.mu.Unlock()
		cancel(nil)
		return nil, nil, fmt.Errorf("%w: request %q is already watched", domain.ErrConflict, requestID)
	}
	canceller.watches[key] = cancel
	canceller.mu.Unlock()

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			canceller.mu.Lock()
			delete(canceller.watches, key)
			canceller.mu.Unlock()
			cancel(nil)
		})
	}
	return turnCtx, release, nil
}

// Cancel stops the watched turn; an unknown request is domain.ErrNotFound.
func (canceller *MemoryTurnCanceller) Cancel(ctx context.Context, tenantID int64, requestID, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	canceller.mu.Lock()
	cancel := canceller.watches[streamKey{tenantID, requestID}]
	canceller.mu.Unlock()
	if cancel == nil {
		return fmt.Errorf("%w: no running turn for request %q", domain.ErrNotFound, requestID)
	}
	cancel(fmt.Errorf("%w: %s", domain.ErrTurnCanceled, reason))
	return nil
}
