package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// CallOutcomeStore records the outcome of every non-read-only call a turn's tool loop sends:
// intent before effect, so a call without a record never left. The owner's writes present its
// TurnOwner and are refused with domain.ErrFenceLost once the fence moved on; a fenced-out
// worker may only retract or report its own dispatch, by the fence it dispatched under. A status
// moves only toward resolution, so a record that is completed or failed never changes again.
type CallOutcomeStore interface {
	// Intend records the call in flight before it is sent, with its deadline the store clock plus
	// the intent's window. An existing record is returned unchanged with created false; an
	// idempotency key that already names another call is domain.ErrConflict.
	Intend(ctx context.Context, owner domain.TurnOwner, intent domain.CallIntent) (outcome domain.CallOutcome, created bool, err error)
	// Redispatch moves an open record back in flight under the owner's fence, for a call whose
	// composition declares that its provider deduplicates by the idempotency key.
	Redispatch(ctx context.Context, owner domain.TurnOwner, key domain.CallOutcomeKey, window time.Duration) (domain.CallOutcome, error)
	// Accept records the owner's in-flight call accepted asynchronously, its acceptance in the
	// result object and its deadline extended to the store clock plus window.
	Accept(ctx context.Context, owner domain.TurnOwner, key domain.CallOutcomeKey, record domain.CallRecord, window time.Duration) (domain.CallOutcome, error)
	// SettleAndAppend records the definite outcome of the owner's open call and appends its
	// observation to the loop journal in one transaction; a failure leaves both unchanged. The
	// journal's first writer wins as LoopJournal.Append's does.
	SettleAndAppend(ctx context.Context, owner domain.TurnOwner, settlement domain.CallSettlement, key domain.LoopKey, entry domain.LoopEntry) (domain.LoopEntry, error)
	// MarkUnknown moves the owner's in-flight or accepted call to unknown; no observation is made up.
	MarkUnknown(ctx context.Context, owner domain.TurnOwner, key domain.CallOutcomeKey) error
	// Retract removes an in-flight record a worker wrote under dispatchFence for a call that never
	// left; a record that moved on is left as it is.
	Retract(ctx context.Context, key domain.CallOutcomeKey, dispatchFence int64) error
	// Report settles an open record a fenced-out worker dispatched under dispatchFence from the
	// definite result it received late. It records evidence, not turn progress.
	Report(ctx context.Context, settlement domain.CallSettlement, dispatchFence int64) error
	// Find reads one record; found is false when the call never left.
	Find(ctx context.Context, key domain.CallOutcomeKey) (outcome domain.CallOutcome, found bool, err error)
	// Record reads the digest-verified result object of a settled or accepted record.
	Record(ctx context.Context, ref domain.ObjectRef) (domain.CallRecord, error)
	// List reads every record of one turn, oldest dispatch first.
	List(ctx context.Context, tenantID int64, requestID string) ([]domain.CallOutcome, error)
	// Resolve settles an open record from a reconciliation source a person asked, while the turn
	// stands held or ended at turnFence; any other turn state or fence is domain.ErrConflict.
	Resolve(ctx context.Context, settlement domain.CallSettlement, resolver domain.PrincipalRef, turnFence int64) (domain.CallOutcome, error)
	// Attest records a person's attestation of an open record once its deadline passed by the
	// store clock, while the turn stands held or ended at the attestation's fence, and settles the
	// record when Persons distinct people agree. An identical repeat is a no-op; a conflicting one
	// is domain.ErrConflict.
	Attest(ctx context.Context, attestation domain.CallAttestation) (domain.AttestationResult, error)
}

// ToolOutcomeLookup is a product's answer, by idempotency key, about a call its provider took:
// completed with the tool's result, failed, or unknown. It never sends the call.
type ToolOutcomeLookup interface {
	Lookup(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) (domain.CallReport, error)
}

// ToolOutcomeReconciler settles a dispatched call whose result was never observed without
// sending it again: from the tool's effect when it declares VerifyEffect, else from the composed
// ToolOutcomeLookup when the composition declares lookup support. A completed answer passes the
// same output validation and guardrails as a sent call's. Traits reports what the trusted
// composition declares about a tool.
type ToolOutcomeReconciler interface {
	Reconcile(ctx context.Context, call domain.ToolCall) (domain.CallReport, error)
	Traits(toolID string) domain.ToolOutcomeTraits
}

// OperationSeparation decides who may attest a call's unknown outcome. Required reports whether
// attesting the call needs a second authorized person, as a high-risk tool's always does; Permit
// refuses a person the tenant separates from attesting it, such as an author of the running
// release. An error from either refuses the attestation.
type OperationSeparation interface {
	Required(ctx context.Context, tenantID int64, agentID string, call domain.CallOutcome, at time.Time) (bool, error)
	Permit(ctx context.Context, tenantID int64, agentID string, call domain.CallOutcome, person domain.PrincipalRef, at time.Time) error
}
