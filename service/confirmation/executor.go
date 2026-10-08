package confirmation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// DefaultLease is one executor claim; it is renewed every third of itself.
const DefaultLease = time.Minute

// Executor decides and runs confirmations. It re-authorizes maker and decider
// against current state before running the stored payload exactly once under a
// fenced lease. A run that loses its claim is never re-run: it becomes unknown
// and waits for a person to reconcile it.
type Executor struct {
	// Store and Checker are required; Runner and OnFailure only to run.
	Store   *TableStore
	Checker contract.MCPConfirmationChecker
	Runner  contract.MCPConfirmedRunner
	// OnFailure receives the internal cause of a recorded failure, which people
	// see only as the client-safe reason.
	OnFailure func(domain.MCPConfirmationKey, error)
	// Lease defaults to DefaultLease.
	Lease time.Duration
	// Reason maps a failure to the client-safe text stored with it; nil uses a generic one.
	Reason func(error) string
}

func (e *Executor) readyToDecide() error {
	if e.Store == nil || e.Checker == nil {
		return fmt.Errorf("%w: confirmation decisions need a store and checker", domain.ErrNotReady)
	}
	return nil
}

func (e *Executor) readyToRun() error {
	if err := e.readyToDecide(); err != nil {
		return err
	}
	if e.Runner == nil || e.OnFailure == nil {
		return fmt.Errorf("%w: confirmation execution needs a runner and failure sink", domain.ErrNotReady)
	}
	return nil
}

func (e *Executor) lease() time.Duration {
	if e.Lease > 0 {
		return e.Lease
	}
	return DefaultLease
}

// Decide authorizes decider and records the decision. A confirmation owned by
// a maker-checker request is decided through keel's approval service instead.
func (e *Executor) Decide(ctx context.Context, key domain.MCPConfirmationKey, decider domain.PrincipalRef, approve bool, note string) (domain.MCPConfirmation, error) {
	if err := e.readyToDecide(); err != nil {
		return domain.MCPConfirmation{}, err
	}
	stored, err := e.Store.Get(ctx, key)
	if err != nil {
		return domain.MCPConfirmation{}, err
	}
	if stored.ApprovalRequired {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: confirmation %d requires maker-checker approval", domain.ErrForbidden, key.ID)
	}
	if err = e.Checker.Decider(ctx, stored, decider); err != nil {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: %w", domain.ErrForbidden, err)
	}
	if err = e.Store.Decide(ctx, key, decider, approve, note); err != nil {
		return domain.MCPConfirmation{}, err
	}
	return e.Store.Get(ctx, key)
}

// Answer handles the call that carries the maker's elicitation answer. The
// state only names a confirmation; it must be this maker's and client's, for
// this tool and action. An approval runs the action before returning.
func (e *Executor) Answer(ctx context.Context, call domain.MCPToolCall, maker domain.PrincipalRef, clientRef, tool, digest string) (domain.MCPConfirmation, error) {
	if err := e.readyToRun(); err != nil {
		return domain.MCPConfirmation{}, err
	}
	id, approve, ok := Answered(call)
	if !ok {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: the call answers no confirmation", domain.ErrNotFound)
	}
	key := domain.MCPConfirmationKey{TenantID: call.Caller.TenantID, ID: id}
	stored, err := e.Store.Get(ctx, key)
	if err != nil {
		return domain.MCPConfirmation{}, err
	}
	if stored.Maker != maker || stored.ClientRef != clientRef || stored.Tool != tool ||
		stored.PayloadDigest != digest || stored.Channel != domain.MCPConfirmationElicitation {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: confirmation %d", domain.ErrNotFound, id)
	}
	if stored.Status != domain.MCPConfirmationPending || stored.Expired {
		return stored, nil
	}
	if stored, err = e.Decide(ctx, key, maker, approve, "answered in the MCP client"); err != nil {
		return domain.MCPConfirmation{}, err
	}
	if stored.Status != domain.MCPConfirmationApproved {
		return stored, nil
	}
	return e.Execute(ctx, key)
}

// Reconcile authorizes reconciler as a decider and records the verified outcome.
func (e *Executor) Reconcile(ctx context.Context, key domain.MCPConfirmationKey, reconciler domain.PrincipalRef, outcome domain.MCPConfirmationStatus, note string) (domain.MCPConfirmation, error) {
	if err := e.readyToDecide(); err != nil {
		return domain.MCPConfirmation{}, err
	}
	stored, err := e.Store.Get(ctx, key)
	if err != nil {
		return domain.MCPConfirmation{}, err
	}
	if err = e.Checker.Decider(ctx, stored, reconciler); err != nil {
		return domain.MCPConfirmation{}, fmt.Errorf("%w: %w", domain.ErrForbidden, err)
	}
	if err = e.Store.Reconcile(ctx, key, reconciler, outcome, note); err != nil {
		return domain.MCPConfirmation{}, err
	}
	return e.Store.Get(ctx, key)
}

// RunApproved executes a bounded batch of approved confirmations: inbox
// decisions and any run interrupted between decision and claim.
func (e *Executor) RunApproved(ctx context.Context, limit int) (int, error) {
	if err := e.readyToRun(); err != nil {
		return 0, err
	}
	keys, err := e.Store.Approved(ctx, limit)
	if err != nil {
		return 0, err
	}
	var errs []error
	for done, key := range keys {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		if _, err := e.Execute(ctx, key); err != nil {
			errs = append(errs, err)
		}
	}
	return len(keys), errors.Join(errs...)
}

// Execute runs one approved confirmation if this call claims it, and returns
// the confirmation as it stands afterwards. A failed action is a recorded
// outcome, not an error; the error reports only what could not be recorded.
func (e *Executor) Execute(ctx context.Context, key domain.MCPConfirmationKey) (domain.MCPConfirmation, error) {
	if err := e.readyToRun(); err != nil {
		return domain.MCPConfirmation{}, err
	}
	fence, claimed, err := e.Store.Claim(ctx, key, e.lease())
	if err != nil {
		return domain.MCPConfirmation{}, err
	}
	var runErr error
	if claimed {
		runErr = e.run(ctx, key, fence)
	}
	stored, err := e.Store.Get(context.WithoutCancel(ctx), key)
	return stored, errors.Join(runErr, err)
}

func (e *Executor) run(ctx context.Context, key domain.MCPConfirmationKey, fence int64) error {
	stored, err := e.Store.Get(ctx, key)
	if err == nil {
		err = e.authorize(ctx, stored)
	}
	if err != nil {
		return e.fail(ctx, key, fence, domain.MCPConfirmationFailed, err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	stop := e.keepClaim(runCtx, cancel, key, fence)
	result, err := e.Runner.RunConfirmed(runCtx, stored)
	if stop() {
		return fmt.Errorf("%w: confirmation %d lost its claim while running", domain.ErrEffectUnknown, key.ID)
	}
	switch {
	case ctx.Err() != nil:
		return e.fail(ctx, key, fence, domain.MCPConfirmationUnknown, fmt.Errorf("%w: canceled while running: %w", domain.ErrEffectUnknown, ctx.Err()))
	case errors.Is(err, domain.ErrEffectUnknown):
		return e.fail(ctx, key, fence, domain.MCPConfirmationUnknown, err)
	case err != nil:
		return e.fail(ctx, key, fence, domain.MCPConfirmationFailed, err)
	}
	if len(result) > MaxDocumentBytes || (len(result) > 0 && !json.Valid(result)) {
		// The runner reported success, so the effect likely happened; unknown keeps it from running twice.
		return e.fail(ctx, key, fence, domain.MCPConfirmationUnknown,
			fmt.Errorf("%w: confirmation %d ran but its result is not storable JSON: %w", domain.ErrEffectUnknown, key.ID, domain.ErrValidation))
	}
	return e.Store.Complete(context.WithoutCancel(ctx), key, fence, domain.MCPConfirmationExecuted, result, "")
}

// authorize checks the maker and the recorded decider as they stand now.
func (e *Executor) authorize(ctx context.Context, stored domain.MCPConfirmation) error {
	if len(stored.Payload) == 0 {
		return fmt.Errorf("%w: the stored action is gone", domain.ErrForbidden)
	}
	if stored.Decider.Kind == "" || stored.Decider.ID == "" {
		return fmt.Errorf("%w: no recorded decider", domain.ErrForbidden)
	}
	if err := e.Checker.Maker(ctx, stored); err != nil {
		return fmt.Errorf("%w: the maker may no longer run it: %w", domain.ErrForbidden, err)
	}
	if err := e.Checker.Decider(ctx, stored, stored.Decider); err != nil {
		return fmt.Errorf("%w: the decider may no longer approve it: %w", domain.ErrForbidden, err)
	}
	return nil
}

func (e *Executor) fail(ctx context.Context, key domain.MCPConfirmationKey, fence int64, status domain.MCPConfirmationStatus, cause error) error {
	e.OnFailure(key, cause)
	return e.Store.Complete(context.WithoutCancel(ctx), key, fence, status, nil, e.reason(cause))
}

func (e *Executor) reason(cause error) string {
	if e.Reason != nil {
		return e.Reason(cause)
	}
	switch {
	case errors.Is(cause, domain.ErrEffectUnknown):
		return "The outcome is unknown; check the target system and reconcile it."
	case errors.Is(cause, domain.ErrForbidden):
		return "Not run: the action is no longer authorized."
	}
	return "The action failed."
}

// keepClaim renews the lease while the action runs; losing the claim cancels
// the run. The returned stop reports whether the claim was lost.
func (e *Executor) keepClaim(ctx context.Context, cancel context.CancelFunc, key domain.MCPConfirmationKey, fence int64) func() bool {
	done, finished := make(chan struct{}), make(chan struct{})
	lost := false
	ticker := time.NewTicker(e.lease() / 3)
	go func() {
		defer close(finished)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := e.Store.Renew(ctx, key, fence, e.lease()); err != nil {
					select {
					case <-done:
						return
					default:
					}
					e.OnFailure(key, fmt.Errorf("renew the claim on confirmation %d: %w", key.ID, err))
					lost = true
					cancel()
					return
				}
			}
		}
	}()
	return func() bool {
		close(done)
		<-finished
		cancel()
		return lost
	}
}
