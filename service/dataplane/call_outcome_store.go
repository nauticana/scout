package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/keel/common"
	keelmodel "github.com/nauticana/keel/model"
	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/confirmation"
)

const (
	qOutcomeIntend         = "scout_call_outcome_intend"
	qOutcomeRedispatch     = "scout_call_outcome_redispatch"
	qOutcomeAccept         = "scout_call_outcome_accept"
	qOutcomeLink           = "scout_call_outcome_link_confirmation"
	qOutcomeOwnerGuard     = "scout_call_outcome_owner_guard"
	qOutcomeSettle         = "scout_call_outcome_settle"
	qOutcomeJournal        = "scout_call_outcome_journal"
	qOutcomeMarkUnknown    = "scout_call_outcome_mark_unknown"
	qOutcomeRetract        = "scout_call_outcome_retract"
	qOutcomeReport         = "scout_call_outcome_report"
	qOutcomeFind           = "scout_call_outcome_find"
	qOutcomeList           = "scout_call_outcome_list"
	qOutcomeResolve        = "scout_call_outcome_resolve"
	qOutcomeAttestTouch    = "scout_call_outcome_attest_touch"
	qOutcomeAttestState    = "scout_call_outcome_attest_state"
	qOutcomeAttestations   = "scout_call_outcome_attestations"
	qOutcomeAttestInsert   = "scout_call_outcome_attest_insert"
	qOutcomeAttestResolve  = "scout_call_outcome_attest_resolve"
	qOutcomeConfirmSettled = "scout_call_outcome_confirmation_settled"

	// DefaultOutcomeAttestGrace is added to a call's dispatch window for its outcome deadline.
	DefaultOutcomeAttestGrace = time.Minute
	// maxTurnOutcomes bounds one turn's outcome read; a turn's calls are bounded well below it.
	maxTurnOutcomes = 4096
	// callOutcomeCatalogID binds the outcome queries to another store's transaction.
	callOutcomeCatalogID = "scout.dataplane.call_outcome"
)

const outcomeColumns = `
SELECT o.request_id, o.execution_step_id, o.call_id, o.idempotency_key, o.tool_id, o.tool_version, o.origin_code,
       o.status_code, o.dispatch_fence, o.dispatched_at, o.deadline_at, o.deadline_at <= CURRENT_TIMESTAMP,
       COALESCE(o.result_uri, ''), COALESCE(o.result_digest, ''), COALESCE(o.resolution_code, ''),
       COALESCE(o.resolved_fence, 0), COALESCE(o.resolver_kind, ''), COALESCE(o.resolver_id, ''), o.resolved_at,
       COALESCE(o.note, ''), COALESCE(o.evidence_uri, ''), COALESCE(o.evidence_digest, '')
  FROM turn_call_outcome o`

// outcomeKey names one record by its primary key; its four placeholders follow the statement's own.
const outcomeKey = `partner_id = ? AND request_id = ? AND execution_step_id = ? AND call_id = ?`

// turnUnworkedAt matches a turn no worker runs, held or ended, at the fence a person saw.
const turnUnworkedAt = `
SELECT 1 FROM conversation_turn t
 WHERE t.partner_id = o.partner_id AND t.request_id = o.request_id AND t.fence = ? AND t.status_code IN (` + sqlTurnUnworked + `)`

var callOutcomeQueries = map[string]string{
	qOutcomeIntend: ownedBy + `
written AS (
INSERT INTO turn_call_outcome (partner_id, request_id, execution_step_id, call_id, idempotency_key, tool_id, tool_version,
                               origin_code, status_code, dispatch_fence, dispatched_at, deadline_at)
SELECT ?, ?, ?, ?, ?, ?, ?, ` + sqlCallFromAgent + `, ` + sqlCallInFlight + `, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP + make_interval(secs => ?)
  FROM owner
ON CONFLICT DO NOTHING
RETURNING call_id)` + fencedWrite,
	qOutcomeRedispatch: ownedBy + `
written AS (
UPDATE turn_call_outcome
   SET status_code = ` + sqlCallInFlight + `, dispatch_fence = ?, dispatched_at = CURRENT_TIMESTAMP,
       deadline_at = CURRENT_TIMESTAMP + make_interval(secs => ?), result_uri = NULL, result_digest = NULL
 WHERE ` + outcomeKey + ` AND status_code IN (` + sqlCallOpen + `) AND EXISTS (SELECT 1 FROM owner)
RETURNING call_id)` + fencedWrite,
	qOutcomeAccept: ownedBy + `
written AS (
UPDATE turn_call_outcome
   SET status_code = ` + sqlCallAccepted + `, result_uri = ?, result_digest = ?,
       deadline_at = GREATEST(deadline_at, CURRENT_TIMESTAMP + make_interval(secs => ?))
 WHERE ` + outcomeKey + ` AND status_code = ` + sqlCallInFlight + ` AND dispatch_fence = ? AND EXISTS (SELECT 1 FROM owner)
RETURNING call_id)` + fencedWrite,
	// The composite foreign key to mcp_confirmation keeps the link inside the call's tenant.
	qOutcomeLink: `
INSERT INTO turn_call_confirmation (partner_id, request_id, execution_step_id, call_id, mcp_confirmation_id)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING`,
	qOutcomeOwnerGuard: turnOwnerGuard,
	qOutcomeSettle: `
UPDATE turn_call_outcome
   SET status_code = ?, resolution_code = ?, result_uri = ?, result_digest = ?, resolved_fence = ?, resolved_at = CURRENT_TIMESTAMP
 WHERE ` + outcomeKey + ` AND status_code IN (` + sqlCallOpen + `)
RETURNING call_id`,
	// The owner's guarded touch opens the settlement's transaction, so the entry needs no guard of its own.
	qOutcomeJournal: `
INSERT INTO step_loop_entry (partner_id, request_id, execution_step_id, entry_no, entry_uri, entry_digest)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING
RETURNING entry_no`,
	qLoopEntryGet: loopJournalQueries[qLoopEntryGet],
	qOutcomeMarkUnknown: ownedBy + `
written AS (
UPDATE turn_call_outcome SET status_code = ` + sqlCallUnknown + `
 WHERE ` + outcomeKey + ` AND status_code IN (` + sqlCallInFlight + `, ` + sqlCallAccepted + `) AND EXISTS (SELECT 1 FROM owner)
RETURNING call_id)` + fencedWrite,
	qOutcomeRetract: `
DELETE FROM turn_call_outcome o
 WHERE ` + outcomeKey + ` AND status_code = ` + sqlCallInFlight + ` AND dispatch_fence = ?
   AND NOT EXISTS (SELECT 1 FROM turn_call_attestation a
                    WHERE a.partner_id = o.partner_id AND a.request_id = o.request_id
                      AND a.execution_step_id = o.execution_step_id AND a.call_id = o.call_id)
RETURNING call_id`,
	// A fenced-out worker settles only the record of its own dispatch: evidence, not turn progress.
	qOutcomeReport: `
UPDATE turn_call_outcome
   SET status_code = ?, resolution_code = ` + sqlResolvedByWorker + `, result_uri = ?, result_digest = ?,
       resolved_fence = dispatch_fence, resolved_at = CURRENT_TIMESTAMP
 WHERE ` + outcomeKey + ` AND status_code IN (` + sqlCallOpen + `) AND dispatch_fence = ?
RETURNING call_id`,
	qOutcomeFind: outcomeColumns + `
 WHERE o.partner_id = ? AND o.request_id = ? AND o.execution_step_id = ? AND o.call_id = ?`,
	qOutcomeList: outcomeColumns + `
 WHERE o.partner_id = ? AND o.request_id = ?
 ORDER BY o.dispatched_at, o.execution_step_id, o.call_id
 LIMIT ?`,
	qOutcomeResolve: `
UPDATE turn_call_outcome o
   SET status_code = ?, resolution_code = ?, result_uri = ?, result_digest = ?, resolved_fence = ?,
       resolver_kind = ?, resolver_id = ?, resolved_at = CURRENT_TIMESTAMP
 WHERE o.partner_id = ? AND o.request_id = ? AND o.execution_step_id = ? AND o.call_id = ?
   AND o.status_code IN (` + sqlCallOpen + `) AND EXISTS (` + turnUnworkedAt + `)
RETURNING call_id`,
	// The attestation's conditional touch of the record it settles orders two people attesting at once.
	qOutcomeAttestTouch: `
UPDATE turn_call_outcome o SET status_code = o.status_code
 WHERE o.partner_id = ? AND o.request_id = ? AND o.execution_step_id = ? AND o.call_id = ?
   AND o.status_code IN (` + sqlCallOpen + `) AND o.deadline_at <= CURRENT_TIMESTAMP AND EXISTS (` + turnUnworkedAt + `)
RETURNING call_id`,
	qOutcomeAttestState: `
SELECT o.status_code, o.deadline_at <= CURRENT_TIMESTAMP, COALESCE(o.resolution_code, ''),
       COALESCE(o.resolver_kind, ''), COALESCE(o.resolver_id, ''),
       EXISTS (` + turnUnworkedAt + `)
  FROM turn_call_outcome o
 WHERE o.partner_id = ? AND o.request_id = ? AND o.execution_step_id = ? AND o.call_id = ?`,
	qOutcomeAttestations: `
SELECT attester_kind, attester_id, outcome_code, note, COALESCE(evidence_uri, ''), COALESCE(evidence_digest, '')
  FROM turn_call_attestation
 WHERE ` + outcomeKey,
	qOutcomeAttestInsert: `
INSERT INTO turn_call_attestation (partner_id, request_id, execution_step_id, call_id, attester_kind, attester_id,
                                   outcome_code, note, evidence_uri, evidence_digest)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING`,
	qOutcomeAttestResolve: `
UPDATE turn_call_outcome
   SET status_code = ?, resolution_code = ` + sqlResolvedByAttestation + `, resolved_fence = ?,
       resolver_kind = ?, resolver_id = ?, note = ?, evidence_uri = ?, evidence_digest = ?, resolved_at = CURRENT_TIMESTAMP
 WHERE ` + outcomeKey + ` AND status_code IN (` + sqlCallOpen + `)
RETURNING call_id`,
	// A reconciled confirmation settles the open calls linked to it inside its tenant.
	qOutcomeConfirmSettled: `
UPDATE turn_call_outcome o
   SET status_code = ?, resolution_code = ` + sqlResolvedByConfirmation + `, resolver_kind = ?, resolver_id = ?,
       resolved_at = CURRENT_TIMESTAMP,
       resolved_fence = (SELECT t.fence FROM conversation_turn t WHERE t.partner_id = o.partner_id AND t.request_id = o.request_id)
  FROM turn_call_confirmation link
 WHERE link.partner_id = ? AND link.mcp_confirmation_id = ?
   AND o.partner_id = link.partner_id AND o.request_id = link.request_id
   AND o.execution_step_id = link.execution_step_id AND o.call_id = link.call_id
   AND o.status_code IN (` + sqlCallOpen + `)
RETURNING o.call_id`,
}

// TableCallOutcomeStore is the CallOutcomeStore over turn_call_outcome. Result objects and
// journal entries live in object storage, the rows keep their URI and digest. The owner's writes
// carry the guarded touch of its turn row, so a hold or preemption linearizes before or after
// each, and the record of a call that never left is the only one ever removed.
type TableCallOutcomeStore struct {
	DB      port.DatabaseRepository
	Objects ObjectStateCodec

	once sync.Once
	qs   port.QueryService
}

var (
	_ contract.CallOutcomeStore    = (*TableCallOutcomeStore)(nil)
	_ confirmation.ReconciledCalls = (*TableCallOutcomeStore)(nil)
)

func (store *TableCallOutcomeStore) queries(ctx context.Context) (port.QueryService, error) {
	if store.DB == nil || store.Objects == nil {
		return nil, fmt.Errorf("%w: the call outcome store needs a database and an object codec", domain.ErrValidation)
	}
	store.once.Do(func() { store.qs = store.DB.GetQueryService(ctx, callOutcomeQueries) })
	if store.qs == nil {
		return nil, fmt.Errorf("call outcome store: query service is required")
	}
	return store.qs, nil
}

func keyArgs(key domain.CallOutcomeKey) []any {
	return []any{key.TenantID, key.RequestID, key.ExecutionStepID, key.CallID}
}

func ownsKey(owner domain.TurnOwner, key domain.CallOutcomeKey) error {
	if err := owner.Check(); err != nil {
		return err
	}
	if err := key.Validate(); err != nil {
		return err
	}
	if !owner.Owns(key.TenantID, key.RequestID) {
		return fmt.Errorf("%w: the owner of turn %q cannot write a call of turn %q", domain.ErrFenceLost, owner.RequestID, key.RequestID)
	}
	return nil
}

// windowSeconds rounds a dispatch window up to whole seconds of at least one.
func windowSeconds(window time.Duration) (int64, error) {
	if window <= 0 || window > 30*24*time.Hour {
		return 0, fmt.Errorf("%w: a call's dispatch window lies within (0, 30 days]", domain.ErrValidation)
	}
	return int64((window-1)/time.Second + 1), nil
}

// Intend records the owner's call in flight before it is sent.
func (store *TableCallOutcomeStore) Intend(ctx context.Context, owner domain.TurnOwner, intent domain.CallIntent) (domain.CallOutcome, bool, error) {
	qs, err := store.queries(ctx)
	if err != nil {
		return domain.CallOutcome{}, false, err
	}
	if err = ownsKey(owner, intent.Key); err != nil {
		return domain.CallOutcome{}, false, err
	}
	if strings.TrimSpace(intent.IdempotencyKey) == "" || len(intent.IdempotencyKey) > 200 ||
		strings.TrimSpace(intent.Tool.ToolID) == "" || strings.TrimSpace(intent.Tool.Version) == "" {
		return domain.CallOutcome{}, false, fmt.Errorf("%w: an intent needs an idempotency key of at most 200 bytes and a tool version", domain.ErrValidation)
	}
	seconds, err := windowSeconds(intent.Window)
	if err != nil {
		return domain.CallOutcome{}, false, err
	}
	ctx = context.WithoutCancel(ctx)
	args := append(keyArgs(intent.Key), intent.IdempotencyKey, intent.Tool.ToolID, intent.Tool.Version, owner.Fence, seconds)
	result, err := qs.Query(ctx, qOutcomeIntend, ownerArgs(owner, args...)...)
	if err != nil {
		return domain.CallOutcome{}, false, fmt.Errorf("record call intent: %w", err)
	}
	created, err := fencedOutcome(result, owner)
	if err != nil {
		return domain.CallOutcome{}, false, err
	}
	outcome, found, err := store.Find(ctx, intent.Key)
	if err != nil {
		return domain.CallOutcome{}, false, err
	}
	if !found {
		return domain.CallOutcome{}, false, fmt.Errorf("%w: idempotency key %q already names another call", domain.ErrConflict, intent.IdempotencyKey)
	}
	if !created && (outcome.IdempotencyKey != intent.IdempotencyKey || outcome.Tool != intent.Tool) {
		return domain.CallOutcome{}, false, fmt.Errorf("%w: call %q was recorded for another tool or key", domain.ErrConflict, intent.Key.CallID)
	}
	return outcome, created, nil
}

// Redispatch moves an open record back in flight under the owner's fence.
func (store *TableCallOutcomeStore) Redispatch(ctx context.Context, owner domain.TurnOwner, key domain.CallOutcomeKey, window time.Duration) (domain.CallOutcome, error) {
	qs, err := store.queries(ctx)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	if err = ownsKey(owner, key); err != nil {
		return domain.CallOutcome{}, err
	}
	seconds, err := windowSeconds(window)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	ctx = context.WithoutCancel(ctx)
	result, err := qs.Query(ctx, qOutcomeRedispatch, ownerArgs(owner, append([]any{owner.Fence, seconds}, keyArgs(key)...)...)...)
	if err != nil {
		return domain.CallOutcome{}, fmt.Errorf("redispatch call: %w", err)
	}
	return store.afterOwnerWrite(ctx, result, owner, key, "redispatch")
}

// Accept records the owner's in-flight call accepted, its acceptance stored and its confirmation linked.
func (store *TableCallOutcomeStore) Accept(ctx context.Context, owner domain.TurnOwner, key domain.CallOutcomeKey, record domain.CallRecord, window time.Duration) (domain.CallOutcome, error) {
	if _, err := store.queries(ctx); err != nil {
		return domain.CallOutcome{}, err
	}
	if err := ownsKey(owner, key); err != nil {
		return domain.CallOutcome{}, err
	}
	if record.Acceptance == nil {
		return domain.CallOutcome{}, fmt.Errorf("%w: an accepted call needs its acceptance", domain.ErrValidation)
	}
	if err := record.Acceptance.Validate(); err != nil {
		return domain.CallOutcome{}, err
	}
	seconds, err := windowSeconds(window)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	ref, err := store.dehydrate(ctx, key, record)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := store.DB.BeginTx(ctx, callOutcomeQueries)
	if err != nil {
		return domain.CallOutcome{}, fmt.Errorf("accept call: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	args := append([]any{ref.URI, ref.Digest, seconds}, keyArgs(key)...)
	result, err := tx.Query(ctx, qOutcomeAccept, ownerArgs(owner, append(args, owner.Fence)...)...)
	if err != nil {
		return domain.CallOutcome{}, fmt.Errorf("accept call: %w", err)
	}
	accepted, err := fencedOutcome(result, owner)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	if !accepted {
		return domain.CallOutcome{}, fmt.Errorf("%w: call %q is not in flight under fence %d", domain.ErrConflict, key.CallID, owner.Fence)
	}
	if id := record.Acceptance.MCPConfirmationID; id > 0 {
		if _, err = tx.Query(ctx, qOutcomeLink, append(keyArgs(key), id)...); err != nil {
			return domain.CallOutcome{}, fmt.Errorf("link call to confirmation %d: %w", id, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.CallOutcome{}, fmt.Errorf("accept call: commit: %w", err)
	}
	committed = true
	return store.mustFind(ctx, key)
}

// SettleAndAppend records the owner's definite outcome and its observation in one transaction.
func (store *TableCallOutcomeStore) SettleAndAppend(ctx context.Context, owner domain.TurnOwner, settlement domain.CallSettlement, key domain.LoopKey, entry domain.LoopEntry) (domain.LoopEntry, error) {
	if _, err := store.queries(ctx); err != nil {
		return domain.LoopEntry{}, err
	}
	if err := settlement.Validate(); err != nil {
		return domain.LoopEntry{}, err
	}
	if err := ownsKey(owner, settlement.Key); err != nil {
		return domain.LoopEntry{}, err
	}
	if err := validateLoopAppend(owner, key, entry); err != nil {
		return domain.LoopEntry{}, err
	}
	if key.ExecutionStepID != settlement.Key.ExecutionStepID || entry.Kind != domain.LoopEntryObservation ||
		entry.Observation == nil || entry.Observation.CallID != settlement.Key.CallID {
		return domain.LoopEntry{}, fmt.Errorf("%w: a settlement appends the observation of its own call", domain.ErrValidation)
	}
	ref, err := store.dehydrate(ctx, settlement.Key, settlement.Record)
	if err != nil {
		return domain.LoopEntry{}, err
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("encode loop entry: %w", err)
	}
	entryRef, err := store.Objects.Dehydrate(ctx, loopEntryName(key, entry.EntryNo), payload)
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("dehydrate loop entry: %w", err)
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := store.DB.BeginTx(ctx, callOutcomeQueries)
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("settle call: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if err = guardOwner(ctx, tx, qOutcomeOwnerGuard, owner); err != nil {
		return domain.LoopEntry{}, err
	}
	settled, err := tx.Query(ctx, qOutcomeSettle, append([]any{string(settlement.Status), string(settlement.Resolution), ref.URI, ref.Digest, owner.Fence},
		keyArgs(settlement.Key)...)...)
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("settle call: %w", err)
	}
	if len(settled.Rows) == 0 {
		if err = store.alreadySettled(ctx, tx, settlement); err != nil {
			return domain.LoopEntry{}, err
		}
	}
	inserted, err := tx.Query(ctx, qOutcomeJournal, key.TenantID, key.RequestID, key.ExecutionStepID, entry.EntryNo, entryRef.URI, entryRef.Digest)
	if err != nil {
		return domain.LoopEntry{}, fmt.Errorf("append loop entry: %w", err)
	}
	stored := entry
	if len(inserted.Rows) == 0 {
		existing, err := tx.Query(ctx, qLoopEntryGet, key.TenantID, key.RequestID, key.ExecutionStepID, entry.EntryNo)
		if err != nil {
			return domain.LoopEntry{}, fmt.Errorf("read loop entry: %w", err)
		}
		if len(existing.Rows) == 0 {
			return domain.LoopEntry{}, fmt.Errorf("loop entry %d of %q disappeared after insert", entry.EntryNo, key.RequestID)
		}
		if stored, err = (&TableLoopJournal{Objects: store.Objects}).hydrate(ctx, existing.Rows[0]); err != nil {
			return domain.LoopEntry{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.LoopEntry{}, fmt.Errorf("settle call: commit: %w", err)
	}
	committed = true
	return stored, nil
}

// alreadySettled accepts a repeat of the same settlement and refuses any other.
func (store *TableCallOutcomeStore) alreadySettled(ctx context.Context, tx port.TxQueryService, settlement domain.CallSettlement) error {
	found, err := tx.Query(ctx, qOutcomeFind, keyArgs(settlement.Key)...)
	if err != nil {
		return fmt.Errorf("read call outcome: %w", err)
	}
	if len(found.Rows) == 0 {
		return fmt.Errorf("%w: call %q was never recorded in flight", domain.ErrConflict, settlement.Key.CallID)
	}
	current, err := decodeOutcome(settlement.Key.TenantID, found.Rows[0])
	if err != nil {
		return err
	}
	if current.Status != settlement.Status || current.Resolution != settlement.Resolution {
		return fmt.Errorf("%w: call %q is already %s by %s", domain.ErrConflict, settlement.Key.CallID, current.Status, current.Resolution)
	}
	return nil
}

// MarkUnknown moves the owner's in-flight or accepted call to unknown.
func (store *TableCallOutcomeStore) MarkUnknown(ctx context.Context, owner domain.TurnOwner, key domain.CallOutcomeKey) error {
	qs, err := store.queries(ctx)
	if err != nil {
		return err
	}
	if err = ownsKey(owner, key); err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	result, err := qs.Query(ctx, qOutcomeMarkUnknown, ownerArgs(owner, keyArgs(key)...)...)
	if err != nil {
		return fmt.Errorf("mark call unknown: %w", err)
	}
	_, err = store.afterOwnerWrite(ctx, result, owner, key, "mark unknown")
	if errors.Is(err, domain.ErrConflict) {
		// Already unknown, or settled meanwhile: either way nothing is made up.
		return nil
	}
	return err
}

// Retract removes the in-flight record of a call that never left.
func (store *TableCallOutcomeStore) Retract(ctx context.Context, key domain.CallOutcomeKey, dispatchFence int64) error {
	qs, err := store.queries(ctx)
	if err != nil {
		return err
	}
	if err = key.Validate(); err != nil {
		return err
	}
	if dispatchFence <= 0 {
		return fmt.Errorf("%w: a retraction names its dispatch fence", domain.ErrValidation)
	}
	if _, err = qs.Query(context.WithoutCancel(ctx), qOutcomeRetract, append(keyArgs(key), dispatchFence)...); err != nil {
		return fmt.Errorf("retract call intent: %w", err)
	}
	return nil
}

// Report settles the record of a fenced-out worker's own dispatch from a late definite result.
func (store *TableCallOutcomeStore) Report(ctx context.Context, settlement domain.CallSettlement, dispatchFence int64) error {
	qs, err := store.queries(ctx)
	if err != nil {
		return err
	}
	if err = settlement.Validate(); err != nil {
		return err
	}
	if settlement.Resolution != domain.ResolvedByWorker || dispatchFence <= 0 {
		return fmt.Errorf("%w: a late report is the worker's, under its dispatch fence", domain.ErrValidation)
	}
	ref, err := store.dehydrate(ctx, settlement.Key, settlement.Record)
	if err != nil {
		return err
	}
	reported, err := qs.Query(context.WithoutCancel(ctx), qOutcomeReport,
		append(append([]any{string(settlement.Status), ref.URI, ref.Digest}, keyArgs(settlement.Key)...), dispatchFence)...)
	if err != nil {
		return fmt.Errorf("report call outcome: %w", err)
	}
	if len(reported.Rows) == 0 {
		return fmt.Errorf("%w: call %q is not open under dispatch fence %d", domain.ErrConflict, settlement.Key.CallID, dispatchFence)
	}
	return nil
}

// Find reads one record.
func (store *TableCallOutcomeStore) Find(ctx context.Context, key domain.CallOutcomeKey) (domain.CallOutcome, bool, error) {
	qs, err := store.queries(ctx)
	if err != nil {
		return domain.CallOutcome{}, false, err
	}
	if err = key.Validate(); err != nil {
		return domain.CallOutcome{}, false, err
	}
	found, err := qs.Query(ctx, qOutcomeFind, keyArgs(key)...)
	if err != nil {
		return domain.CallOutcome{}, false, fmt.Errorf("read call outcome: %w", err)
	}
	if len(found.Rows) == 0 {
		return domain.CallOutcome{}, false, nil
	}
	outcome, err := decodeOutcome(key.TenantID, found.Rows[0])
	return outcome, err == nil, err
}

func (store *TableCallOutcomeStore) mustFind(ctx context.Context, key domain.CallOutcomeKey) (domain.CallOutcome, error) {
	outcome, found, err := store.Find(ctx, key)
	if err == nil && !found {
		err = fmt.Errorf("%w: call %q", domain.ErrNotFound, key.CallID)
	}
	return outcome, err
}

// List reads every record of one turn, oldest dispatch first.
func (store *TableCallOutcomeStore) List(ctx context.Context, tenantID int64, requestID string) ([]domain.CallOutcome, error) {
	qs, err := store.queries(ctx)
	if err != nil {
		return nil, err
	}
	if tenantID <= 0 || strings.TrimSpace(requestID) == "" {
		return nil, fmt.Errorf("%w: tenant and request are required", domain.ErrValidation)
	}
	listed, err := qs.Query(ctx, qOutcomeList, tenantID, requestID, maxTurnOutcomes+1)
	if err != nil {
		return nil, fmt.Errorf("list call outcomes: %w", err)
	}
	if len(listed.Rows) > maxTurnOutcomes {
		return nil, fmt.Errorf("%w: turn %q has more than %d call outcomes", domain.ErrExecutionLimit, requestID, maxTurnOutcomes)
	}
	outcomes := make([]domain.CallOutcome, 0, len(listed.Rows))
	for _, row := range listed.Rows {
		outcome, err := decodeOutcome(tenantID, row)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// Resolve settles an open record from a source a person asked while the turn stands unworked at turnFence.
func (store *TableCallOutcomeStore) Resolve(ctx context.Context, settlement domain.CallSettlement, resolver domain.PrincipalRef, turnFence int64) (domain.CallOutcome, error) {
	qs, err := store.queries(ctx)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	if err = settlement.Validate(); err != nil {
		return domain.CallOutcome{}, err
	}
	if settlement.Resolution != domain.ResolvedByEffect && settlement.Resolution != domain.ResolvedByLookup {
		return domain.CallOutcome{}, fmt.Errorf("%w: a person resolves a call from its effect or its lookup", domain.ErrValidation)
	}
	if resolver.Kind == "" || strings.TrimSpace(resolver.ID) == "" || turnFence <= 0 {
		return domain.CallOutcome{}, fmt.Errorf("%w: a resolution needs its person and the fence they saw", domain.ErrPrincipalUnknown)
	}
	ref, err := store.dehydrate(ctx, settlement.Key, settlement.Record)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	ctx = context.WithoutCancel(ctx)
	args := []any{string(settlement.Status), string(settlement.Resolution), ref.URI, ref.Digest, turnFence, string(resolver.Kind), resolver.ID}
	resolved, err := qs.Query(ctx, qOutcomeResolve, append(append(args, keyArgs(settlement.Key)...), turnFence)...)
	if err != nil {
		return domain.CallOutcome{}, fmt.Errorf("resolve call outcome: %w", err)
	}
	if len(resolved.Rows) == 0 {
		return domain.CallOutcome{}, fmt.Errorf("%w: call %q is not open on a turn held or ended at fence %d", domain.ErrConflict, settlement.Key.CallID, turnFence)
	}
	return store.mustFind(ctx, settlement.Key)
}

// Attest records one person's attestation and settles the record once enough agree.
func (store *TableCallOutcomeStore) Attest(ctx context.Context, attestation domain.CallAttestation) (domain.AttestationResult, error) {
	if _, err := store.queries(ctx); err != nil {
		return domain.AttestationResult{}, err
	}
	if err := attestation.Validate(); err != nil {
		return domain.AttestationResult{}, err
	}
	attestation.Note = strings.TrimSpace(attestation.Note)
	key := attestation.Key
	ctx = context.WithoutCancel(ctx)
	tx, err := store.DB.BeginTx(ctx, callOutcomeQueries)
	if err != nil {
		return domain.AttestationResult{}, fmt.Errorf("attest call: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	touched, err := tx.Query(ctx, qOutcomeAttestTouch, append(keyArgs(key), attestation.TurnFence)...)
	if err != nil {
		return domain.AttestationResult{}, fmt.Errorf("attest call: %w", err)
	}
	if len(touched.Rows) == 0 {
		return store.refusedAttestation(ctx, tx, attestation)
	}
	others, own, err := attestationsOf(ctx, tx, attestation)
	if err != nil {
		return domain.AttestationResult{}, err
	}
	replay := own != nil
	if replay && !sameAttestation(*own, attestation) {
		return domain.AttestationResult{}, fmt.Errorf("%w: %s already attested call %q differently", domain.ErrConflict, attestation.Attester.ID, key.CallID)
	}
	agreeing := 1
	for _, other := range others {
		if other.Outcome != attestation.Outcome {
			return domain.AttestationResult{}, fmt.Errorf("%w: %s attested call %q %s", domain.ErrConflict, other.Attester.ID, key.CallID, other.Outcome)
		}
		agreeing++
	}
	if !replay {
		evidenceURI, evidenceDigest := nullable(attestation.Evidence.URI), nullable(attestation.Evidence.Digest)
		if _, err = tx.Query(ctx, qOutcomeAttestInsert, append(keyArgs(key), string(attestation.Attester.Kind), attestation.Attester.ID,
			string(attestation.Outcome), attestation.Note, evidenceURI, evidenceDigest)...); err != nil {
			return domain.AttestationResult{}, fmt.Errorf("record attestation: %w", err)
		}
	}
	awaits := agreeing < attestation.Persons
	if !awaits {
		resolved, err := tx.Query(ctx, qOutcomeAttestResolve, append([]any{string(attestation.Outcome), attestation.TurnFence,
			string(attestation.Attester.Kind), attestation.Attester.ID, attestation.Note,
			nullable(attestation.Evidence.URI), nullable(attestation.Evidence.Digest)}, keyArgs(key)...)...)
		if err != nil {
			return domain.AttestationResult{}, fmt.Errorf("settle attested call: %w", err)
		}
		if len(resolved.Rows) == 0 {
			return domain.AttestationResult{}, fmt.Errorf("%w: call %q settled meanwhile", domain.ErrConflict, key.CallID)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.AttestationResult{}, fmt.Errorf("attest call: commit: %w", err)
	}
	committed = true
	outcome, err := store.mustFind(ctx, key)
	return domain.AttestationResult{Outcome: outcome, AwaitsSecond: awaits, AlreadyStated: replay}, err
}

// refusedAttestation explains a record the attestation could not touch: an identical repeat of a
// settled attestation is a no-op, anything else names its reason.
func (store *TableCallOutcomeStore) refusedAttestation(ctx context.Context, tx port.TxQueryService, attestation domain.CallAttestation) (domain.AttestationResult, error) {
	key := attestation.Key
	state, err := tx.Query(ctx, qOutcomeAttestState, append([]any{attestation.TurnFence}, keyArgs(key)...)...)
	if err != nil {
		return domain.AttestationResult{}, fmt.Errorf("read attested call: %w", err)
	}
	if len(state.Rows) == 0 {
		return domain.AttestationResult{}, fmt.Errorf("%w: call %q of turn %q", domain.ErrNotFound, key.CallID, key.RequestID)
	}
	row := state.Rows[0]
	status, due, resolution := domain.CallOutcomeStatus(common.AsString(row[0])), common.AsBool(row[1]), domain.CallResolution(common.AsString(row[2]))
	resolver := domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[3])), ID: common.AsString(row[4])}
	switch {
	case status.Definite() && resolution == domain.ResolvedByAttestation && status == attestation.Outcome:
		_, own, err := attestationsOf(ctx, tx, attestation)
		if err != nil {
			return domain.AttestationResult{}, err
		}
		if own != nil && sameAttestation(*own, attestation) {
			outcome, err := store.mustFind(ctx, key)
			return domain.AttestationResult{Outcome: outcome, AlreadyStated: true}, err
		}
		return domain.AttestationResult{}, fmt.Errorf("%w: call %q is already attested %s by %s", domain.ErrConflict, key.CallID, status, resolver.ID)
	case status.Definite():
		return domain.AttestationResult{}, fmt.Errorf("%w: call %q is already %s by %s", domain.ErrConflict, key.CallID, status, resolution)
	case !due:
		return domain.AttestationResult{}, fmt.Errorf("%w: call %q may still be in flight; attest it after its deadline", domain.ErrConflict, key.CallID)
	case !common.AsBool(row[5]):
		return domain.AttestationResult{}, fmt.Errorf("%w: turn %q is not held or ended at fence %d", domain.ErrConflict, key.RequestID, attestation.TurnFence)
	}
	return domain.AttestationResult{}, fmt.Errorf("%w: call %q cannot be attested now", domain.ErrConflict, key.CallID)
}

// attestationsOf splits the call's attestations into other people's and the attester's own.
func attestationsOf(ctx context.Context, qs port.QueryService, attestation domain.CallAttestation) ([]domain.CallAttestation, *domain.CallAttestation, error) {
	listed, err := qs.Query(ctx, qOutcomeAttestations, keyArgs(attestation.Key)...)
	if err != nil {
		return nil, nil, fmt.Errorf("read attestations: %w", err)
	}
	var others []domain.CallAttestation
	var own *domain.CallAttestation
	for _, row := range listed.Rows {
		stated := domain.CallAttestation{
			Key:      attestation.Key,
			Attester: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[0])), ID: common.AsString(row[1])},
			Outcome:  domain.CallOutcomeStatus(common.AsString(row[2])), Note: common.AsString(row[3]),
			Evidence: domain.ObjectRef{URI: common.AsString(row[4]), Digest: common.AsString(row[5])},
		}
		if stated.Attester == attestation.Attester {
			own = &stated
			continue
		}
		others = append(others, stated)
	}
	return others, own, nil
}

func sameAttestation(stated, offered domain.CallAttestation) bool {
	return stated.Outcome == offered.Outcome && stated.Note == strings.TrimSpace(offered.Note) && stated.Evidence == offered.Evidence
}

// ConfirmationReconciledTx settles, inside the transaction that reconciles an MCP confirmation,
// the open calls linked to it: completed for an executed confirmation, failed for a failed one.
func (store *TableCallOutcomeStore) ConfirmationReconciledTx(ctx context.Context, tx port.TxQueryService, key domain.MCPConfirmationKey, outcome domain.MCPConfirmationStatus, reconciler domain.PrincipalRef) error {
	status := domain.CallCompleted
	switch outcome {
	case domain.MCPConfirmationExecuted:
	case domain.MCPConfirmationFailed:
		status = domain.CallFailed
	default:
		return fmt.Errorf("%w: a reconciled confirmation is executed or failed", domain.ErrValidation)
	}
	if key.TenantID <= 0 || key.ID <= 0 || reconciler.Kind == "" || strings.TrimSpace(reconciler.ID) == "" {
		return fmt.Errorf("%w: a reconciled confirmation needs its tenant, id, and reconciler", domain.ErrValidation)
	}
	catalog, ok := tx.(port.TxQueryCatalog)
	if !ok {
		return fmt.Errorf("%w: the reconciling transaction cannot bind the call outcome query catalog", domain.ErrNotReady)
	}
	qs := catalog.QueryService(callOutcomeCatalogID, callOutcomeQueries)
	if qs == nil {
		return fmt.Errorf("%w: the reconciling transaction returned no call outcome query service", domain.ErrNotReady)
	}
	if _, err := qs.Query(ctx, qOutcomeConfirmSettled, string(status), string(reconciler.Kind), reconciler.ID, key.TenantID, key.ID); err != nil {
		return fmt.Errorf("settle calls of confirmation %d: %w", key.ID, err)
	}
	return nil
}

// afterOwnerWrite returns the record a fenced write changed; a write that changed nothing is a conflict.
func (store *TableCallOutcomeStore) afterOwnerWrite(ctx context.Context, result *keelmodel.QueryResult, owner domain.TurnOwner, key domain.CallOutcomeKey, operation string) (domain.CallOutcome, error) {
	written, err := fencedOutcome(result, owner)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	if !written {
		return domain.CallOutcome{}, fmt.Errorf("%w: cannot %s call %q in its current state", domain.ErrConflict, operation, key.CallID)
	}
	return store.mustFind(ctx, key)
}

func (store *TableCallOutcomeStore) dehydrate(ctx context.Context, key domain.CallOutcomeKey, record domain.CallRecord) (domain.ObjectRef, error) {
	return dehydrateCallRecord(ctx, store.Objects, key, record)
}

func dehydrateCallRecord(ctx context.Context, objects ObjectStateCodec, key domain.CallOutcomeKey, record domain.CallRecord) (domain.ObjectRef, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return domain.ObjectRef{}, fmt.Errorf("encode call record: %w", err)
	}
	ref, err := objects.Dehydrate(ctx, callRecordName(key), payload)
	if err != nil {
		return domain.ObjectRef{}, fmt.Errorf("dehydrate call record: %w", err)
	}
	return ref, nil
}

// callRecordName puts the tenant after the object kind, so the record can be admitted as evidence.
func callRecordName(key domain.CallOutcomeKey) string {
	return fmt.Sprintf("call-outcome/%d/%s/%d/%s", key.TenantID, url.PathEscape(key.RequestID), key.ExecutionStepID, url.PathEscape(key.CallID))
}

// Record reads the result object of a settled or accepted record.
func (store *TableCallOutcomeStore) Record(ctx context.Context, ref domain.ObjectRef) (domain.CallRecord, error) {
	if store.Objects == nil {
		return domain.CallRecord{}, fmt.Errorf("%w: the call outcome store needs an object codec", domain.ErrValidation)
	}
	return HydrateCallRecord(ctx, store.Objects, ref)
}

// HydrateCallRecord reads a settled or accepted call's result object, digest-verified.
func HydrateCallRecord(ctx context.Context, objects contract.EvidenceArchive, ref domain.ObjectRef) (domain.CallRecord, error) {
	payload, err := objects.Hydrate(ctx, ref)
	if err != nil {
		return domain.CallRecord{}, fmt.Errorf("hydrate call record: %w", err)
	}
	var record domain.CallRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return domain.CallRecord{}, fmt.Errorf("decode call record: %w", err)
	}
	return record, nil
}

func decodeOutcome(tenantID int64, row []any) (domain.CallOutcome, error) {
	if len(row) < 22 {
		return domain.CallOutcome{}, fmt.Errorf("decode call outcome: expected 22 columns, got %d", len(row))
	}
	outcome := domain.CallOutcome{
		Key: domain.CallOutcomeKey{TenantID: tenantID, RequestID: common.AsString(row[0]), ExecutionStepID: common.AsInt64(row[1]),
			CallID: common.AsString(row[2])},
		IdempotencyKey: common.AsString(row[3]),
		Tool:           domain.ToolReference{ToolID: common.AsString(row[4]), Version: common.AsString(row[5])},
		Origin:         domain.CallOrigin(common.AsString(row[6])), Status: domain.CallOutcomeStatus(common.AsString(row[7])),
		DispatchFence: common.AsInt64(row[8]), DeadlinePassed: common.AsBool(row[11]),
		Result:     domain.ObjectRef{URI: common.AsString(row[12]), Digest: common.AsString(row[13])},
		Resolution: domain.CallResolution(common.AsString(row[14])), ResolvedFence: common.AsInt64(row[15]),
		Resolver: domain.PrincipalRef{Kind: domain.PrincipalKind(common.AsString(row[16])), ID: common.AsString(row[17])},
		Note:     common.AsString(row[19]), Evidence: domain.ObjectRef{URI: common.AsString(row[20]), Digest: common.AsString(row[21])},
	}
	outcome.DispatchedAt, _ = common.AsTimeOK(row[9])
	outcome.DeadlineAt, _ = common.AsTimeOK(row[10])
	outcome.ResolvedAt, _ = common.AsTimeOK(row[18])
	if !outcome.Status.Valid() || !outcome.Origin.Valid() || outcome.Resolution != "" && !outcome.Resolution.Valid() ||
		outcome.Status.Definite() != (outcome.Resolution != "") {
		return domain.CallOutcome{}, fmt.Errorf("%w: call outcome %q does not decode", domain.ErrConflict, outcome.Key.CallID)
	}
	return outcome, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
