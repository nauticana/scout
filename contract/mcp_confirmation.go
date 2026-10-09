package contract

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nauticana/scout/domain"
)

// MCPConfirmationChecker re-authorizes a confirmation against current state.
// The product owns who may make and decide an action; any error refuses.
type MCPConfirmationChecker interface {
	// Maker reports whether the maker may still run the action.
	Maker(ctx context.Context, confirmation domain.MCPConfirmation) error
	// Decider reports whether decider may decide the action now.
	Decider(ctx context.Context, confirmation domain.MCPConfirmation, decider domain.PrincipalRef) error
}

// MCPConfirmedRunner runs a confirmed action's stored payload through the
// product's tool backend. An error wrapping domain.ErrEffectUnknown records the
// outcome as unknown; any other error records it as failed.
type MCPConfirmedRunner interface {
	RunConfirmed(ctx context.Context, confirmation domain.MCPConfirmation) (json.RawMessage, error)
}

// MCPConfirmationStore is the confirmation lifecycle the executor drives. Every
// transition is guarded by the current status and, while running, the fence:
// a refused guard is domain.ErrConflict and changes nothing. Times, expiry, and
// leases come from the store clock.
type MCPConfirmationStore interface {
	// Get reads one confirmation inside its tenant; domain.ErrNotFound otherwise.
	Get(ctx context.Context, key domain.MCPConfirmationKey) (domain.MCPConfirmation, error)
	// Decide records a decision on an unexpired pending confirmation no maker-checker request owns.
	Decide(ctx context.Context, key domain.MCPConfirmationKey, decider domain.PrincipalRef, approve bool, note string) error
	// Approved lists a bounded batch of approved confirmations, oldest decision first.
	Approved(ctx context.Context, limit int) ([]domain.MCPConfirmationKey, error)
	// Claim moves an approved confirmation to executing under a new fence;
	// claimed is false when it was not approved.
	Claim(ctx context.Context, key domain.MCPConfirmationKey, lease time.Duration) (fence int64, claimed bool, err error)
	// Renew extends a claim whose fence still holds.
	Renew(ctx context.Context, key domain.MCPConfirmationKey, fence int64, lease time.Duration) error
	// Complete records executed, failed, or unknown for a claim whose fence still holds.
	Complete(ctx context.Context, key domain.MCPConfirmationKey, fence int64, status domain.MCPConfirmationStatus, result json.RawMessage, reason string) error
	// Reconcile records the verified outcome, executed or failed, of an unknown confirmation.
	Reconcile(ctx context.Context, key domain.MCPConfirmationKey, reconciler domain.PrincipalRef, outcome domain.MCPConfirmationStatus, note string) error
}
