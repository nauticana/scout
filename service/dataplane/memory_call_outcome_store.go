package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/clk"
)

// MemoryCallOutcomeStore is the single-process CallOutcomeStore for tests and embedded runtimes.
// Fences is its fence authority and Journal the loop journal its settlements append to; without
// either every owner write is refused. Objects keeps result records; nil keeps them in memory.
// A turn counts as held or ended for a person's resolution when its fence is not owned.
type MemoryCallOutcomeStore struct {
	Fences  *MemoryTurnFences
	Journal contract.LoopJournal
	Objects ObjectStateCodec
	Now     func() time.Time

	mu           sync.Mutex
	outcomes     map[domain.CallOutcomeKey]*domain.CallOutcome
	records      map[string][]byte
	attestations map[domain.CallOutcomeKey][]domain.CallAttestation
}

var _ contract.CallOutcomeStore = (*MemoryCallOutcomeStore)(nil)

func (store *MemoryCallOutcomeStore) now() time.Time {
	return clk.Now(store.Now)
}

func (store *MemoryCallOutcomeStore) checkOwner(owner domain.TurnOwner, key domain.CallOutcomeKey) error {
	if err := ownsKey(owner, key); err != nil {
		return err
	}
	if store.Fences == nil {
		return fmt.Errorf("%w: memory call outcome store has no fence authority", domain.ErrFenceLost)
	}
	return store.Fences.Check(owner)
}

func (store *MemoryCallOutcomeStore) current(key domain.CallOutcomeKey) domain.CallOutcome {
	outcome := *store.outcomes[key]
	outcome.DeadlinePassed = !store.now().Before(outcome.DeadlineAt)
	return outcome
}

// Intend records the owner's call in flight before it is sent.
func (store *MemoryCallOutcomeStore) Intend(_ context.Context, owner domain.TurnOwner, intent domain.CallIntent) (domain.CallOutcome, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOwner(owner, intent.Key); err != nil {
		return domain.CallOutcome{}, false, err
	}
	if strings.TrimSpace(intent.IdempotencyKey) == "" || strings.TrimSpace(intent.Tool.ToolID) == "" || strings.TrimSpace(intent.Tool.Version) == "" {
		return domain.CallOutcome{}, false, fmt.Errorf("%w: an intent needs an idempotency key and a tool version", domain.ErrValidation)
	}
	if _, err := windowSeconds(intent.Window); err != nil {
		return domain.CallOutcome{}, false, err
	}
	if existing, found := store.outcomes[intent.Key]; found {
		if existing.IdempotencyKey != intent.IdempotencyKey || existing.Tool != intent.Tool {
			return domain.CallOutcome{}, false, fmt.Errorf("%w: call %q was recorded for another tool or key", domain.ErrConflict, intent.Key.CallID)
		}
		return store.current(intent.Key), false, nil
	}
	for key, existing := range store.outcomes {
		if key.TenantID == intent.Key.TenantID && existing.IdempotencyKey == intent.IdempotencyKey {
			return domain.CallOutcome{}, false, fmt.Errorf("%w: idempotency key %q already names another call", domain.ErrConflict, intent.IdempotencyKey)
		}
	}
	if store.outcomes == nil {
		store.outcomes = map[domain.CallOutcomeKey]*domain.CallOutcome{}
	}
	now := store.now()
	store.outcomes[intent.Key] = &domain.CallOutcome{
		Key: intent.Key, IdempotencyKey: intent.IdempotencyKey, Tool: intent.Tool, Origin: domain.CallFromAgent,
		Status: domain.CallInFlight, DispatchFence: owner.Fence, DispatchedAt: now, DeadlineAt: now.Add(intent.Window),
	}
	return store.current(intent.Key), true, nil
}

// Redispatch moves an open record back in flight under the owner's fence.
func (store *MemoryCallOutcomeStore) Redispatch(_ context.Context, owner domain.TurnOwner, key domain.CallOutcomeKey, window time.Duration) (domain.CallOutcome, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOwner(owner, key); err != nil {
		return domain.CallOutcome{}, err
	}
	if _, err := windowSeconds(window); err != nil {
		return domain.CallOutcome{}, err
	}
	outcome, found := store.outcomes[key]
	if !found || !outcome.Status.Open() {
		return domain.CallOutcome{}, fmt.Errorf("%w: call %q is not open", domain.ErrConflict, key.CallID)
	}
	now := store.now()
	outcome.Status, outcome.DispatchFence, outcome.DispatchedAt, outcome.DeadlineAt, outcome.Result = domain.CallInFlight, owner.Fence, now, now.Add(window), domain.ObjectRef{}
	return store.current(key), nil
}

// Accept records the owner's in-flight call accepted.
func (store *MemoryCallOutcomeStore) Accept(ctx context.Context, owner domain.TurnOwner, key domain.CallOutcomeKey, record domain.CallRecord, window time.Duration) (domain.CallOutcome, error) {
	if record.Acceptance == nil {
		return domain.CallOutcome{}, fmt.Errorf("%w: an accepted call needs its acceptance", domain.ErrValidation)
	}
	if err := record.Acceptance.Validate(); err != nil {
		return domain.CallOutcome{}, err
	}
	if _, err := windowSeconds(window); err != nil {
		return domain.CallOutcome{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOwner(owner, key); err != nil {
		return domain.CallOutcome{}, err
	}
	outcome, found := store.outcomes[key]
	if !found || outcome.Status != domain.CallInFlight || outcome.DispatchFence != owner.Fence {
		return domain.CallOutcome{}, fmt.Errorf("%w: call %q is not in flight under fence %d", domain.ErrConflict, key.CallID, owner.Fence)
	}
	ref, err := store.keep(ctx, key, record)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	outcome.Status, outcome.Result = domain.CallAccepted, ref
	if deadline := store.now().Add(window); deadline.After(outcome.DeadlineAt) {
		outcome.DeadlineAt = deadline
	}
	return store.current(key), nil
}

// SettleAndAppend records the owner's definite outcome and appends its observation atomically.
func (store *MemoryCallOutcomeStore) SettleAndAppend(ctx context.Context, owner domain.TurnOwner, settlement domain.CallSettlement, key domain.LoopKey, entry domain.LoopEntry) (domain.LoopEntry, error) {
	if err := settlement.Validate(); err != nil {
		return domain.LoopEntry{}, err
	}
	if store.Journal == nil {
		return domain.LoopEntry{}, fmt.Errorf("%w: memory call outcome store has no journal", domain.ErrFenceLost)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOwner(owner, settlement.Key); err != nil {
		return domain.LoopEntry{}, err
	}
	if key.ExecutionStepID != settlement.Key.ExecutionStepID || entry.Kind != domain.LoopEntryObservation ||
		entry.Observation == nil || entry.Observation.CallID != settlement.Key.CallID {
		return domain.LoopEntry{}, fmt.Errorf("%w: a settlement appends the observation of its own call", domain.ErrValidation)
	}
	outcome, found := store.outcomes[settlement.Key]
	switch {
	case !found:
		return domain.LoopEntry{}, fmt.Errorf("%w: call %q was never recorded in flight", domain.ErrConflict, settlement.Key.CallID)
	case !outcome.Status.Open() && (outcome.Status != settlement.Status || outcome.Resolution != settlement.Resolution):
		return domain.LoopEntry{}, fmt.Errorf("%w: call %q is already %s", domain.ErrConflict, settlement.Key.CallID, outcome.Status)
	}
	ref, err := store.keep(ctx, settlement.Key, settlement.Record)
	if err != nil {
		return domain.LoopEntry{}, err
	}
	// The journal append is the step that can fail; the outcome changes with the entry, as in one transaction.
	stored, err := store.Journal.Append(ctx, owner, key, entry)
	if err != nil {
		if entries, loadErr := store.Journal.Load(ctx, key); loadErr == nil && len(entries) >= entry.EntryNo &&
			sameLoopEntry(entries[entry.EntryNo-1], entry) && outcome.Status.Open() {
			outcome.Status, outcome.Resolution, outcome.Result = settlement.Status, settlement.Resolution, ref
			outcome.ResolvedFence, outcome.ResolvedAt = owner.Fence, store.now()
		}
		return domain.LoopEntry{}, err
	}
	if outcome.Status.Open() {
		outcome.Status, outcome.Resolution, outcome.Result = settlement.Status, settlement.Resolution, ref
		outcome.ResolvedFence, outcome.ResolvedAt = owner.Fence, store.now()
	}
	return stored, nil
}

// MarkUnknown moves the owner's in-flight or accepted call to unknown.
func (store *MemoryCallOutcomeStore) MarkUnknown(_ context.Context, owner domain.TurnOwner, key domain.CallOutcomeKey) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkOwner(owner, key); err != nil {
		return err
	}
	if outcome, found := store.outcomes[key]; found && (outcome.Status == domain.CallInFlight || outcome.Status == domain.CallAccepted) {
		outcome.Status = domain.CallUnknown
	}
	return nil
}

// Retract removes the in-flight record of a call that never left.
func (store *MemoryCallOutcomeStore) Retract(_ context.Context, key domain.CallOutcomeKey, dispatchFence int64) error {
	if err := key.Validate(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if outcome, found := store.outcomes[key]; found && outcome.Status == domain.CallInFlight && outcome.DispatchFence == dispatchFence &&
		len(store.attestations[key]) == 0 {
		delete(store.outcomes, key)
	}
	return nil
}

// Report settles the record of a fenced-out worker's own dispatch.
func (store *MemoryCallOutcomeStore) Report(ctx context.Context, settlement domain.CallSettlement, dispatchFence int64) error {
	if err := settlement.Validate(); err != nil {
		return err
	}
	if settlement.Resolution != domain.ResolvedByWorker || dispatchFence <= 0 {
		return fmt.Errorf("%w: a late report is the worker's, under its dispatch fence", domain.ErrValidation)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	outcome, found := store.outcomes[settlement.Key]
	if !found || !outcome.Status.Open() || outcome.DispatchFence != dispatchFence {
		return fmt.Errorf("%w: call %q is not open under dispatch fence %d", domain.ErrConflict, settlement.Key.CallID, dispatchFence)
	}
	ref, err := store.keep(ctx, settlement.Key, settlement.Record)
	if err != nil {
		return err
	}
	outcome.Status, outcome.Resolution, outcome.Result = settlement.Status, domain.ResolvedByWorker, ref
	outcome.ResolvedFence, outcome.ResolvedAt = dispatchFence, store.now()
	return nil
}

// Find reads one record.
func (store *MemoryCallOutcomeStore) Find(_ context.Context, key domain.CallOutcomeKey) (domain.CallOutcome, bool, error) {
	if err := key.Validate(); err != nil {
		return domain.CallOutcome{}, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, found := store.outcomes[key]; !found {
		return domain.CallOutcome{}, false, nil
	}
	return store.current(key), true, nil
}

// List reads every record of one turn, oldest dispatch first.
func (store *MemoryCallOutcomeStore) List(_ context.Context, tenantID int64, requestID string) ([]domain.CallOutcome, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var outcomes []domain.CallOutcome
	for key := range store.outcomes {
		if key.TenantID == tenantID && key.RequestID == requestID {
			outcomes = append(outcomes, store.current(key))
			if len(outcomes) > maxTurnOutcomes {
				return nil, fmt.Errorf("%w: turn %q has more than %d call outcomes", domain.ErrExecutionLimit, requestID, maxTurnOutcomes)
			}
		}
	}
	slices.SortFunc(outcomes, func(a, b domain.CallOutcome) int {
		if c := a.DispatchedAt.Compare(b.DispatchedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Key.CallID, b.Key.CallID)
	})
	return outcomes, nil
}

// unworked reports whether the turn stands at turnFence with no owner.
func (store *MemoryCallOutcomeStore) unworked(key domain.CallOutcomeKey, turnFence int64) bool {
	if store.Fences == nil {
		return false
	}
	store.Fences.mu.Lock()
	defer store.Fences.mu.Unlock()
	current := store.Fences.turns[streamKey{key.TenantID, key.RequestID}]
	return current.fence == turnFence && !current.owned
}

// Resolve settles an open record from a source a person asked while the turn is unworked at turnFence.
func (store *MemoryCallOutcomeStore) Resolve(ctx context.Context, settlement domain.CallSettlement, resolver domain.PrincipalRef, turnFence int64) (domain.CallOutcome, error) {
	if err := settlement.Validate(); err != nil {
		return domain.CallOutcome{}, err
	}
	if settlement.Resolution != domain.ResolvedByEffect && settlement.Resolution != domain.ResolvedByLookup || resolver.Kind == "" || resolver.ID == "" {
		return domain.CallOutcome{}, fmt.Errorf("%w: a person resolves a call from its effect or its lookup", domain.ErrValidation)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	outcome, found := store.outcomes[settlement.Key]
	if !found || !outcome.Status.Open() || !store.unworked(settlement.Key, turnFence) {
		return domain.CallOutcome{}, fmt.Errorf("%w: call %q is not open on a turn held or ended at fence %d", domain.ErrConflict, settlement.Key.CallID, turnFence)
	}
	ref, err := store.keep(ctx, settlement.Key, settlement.Record)
	if err != nil {
		return domain.CallOutcome{}, err
	}
	outcome.Status, outcome.Resolution, outcome.Result, outcome.Resolver = settlement.Status, settlement.Resolution, ref, resolver
	outcome.ResolvedFence, outcome.ResolvedAt = turnFence, store.now()
	return store.current(settlement.Key), nil
}

// Attest records one person's attestation and settles the record once enough agree.
func (store *MemoryCallOutcomeStore) Attest(_ context.Context, attestation domain.CallAttestation) (domain.AttestationResult, error) {
	if err := attestation.Validate(); err != nil {
		return domain.AttestationResult{}, err
	}
	attestation.Note = strings.TrimSpace(attestation.Note)
	key := attestation.Key
	store.mu.Lock()
	defer store.mu.Unlock()
	outcome, found := store.outcomes[key]
	if !found {
		return domain.AttestationResult{}, fmt.Errorf("%w: call %q of turn %q", domain.ErrNotFound, key.CallID, key.RequestID)
	}
	var own *domain.CallAttestation
	agreeing := 1
	for _, stated := range store.attestations[key] {
		switch {
		case stated.Attester == attestation.Attester:
			own = &stated
		case stated.Outcome != attestation.Outcome:
			return domain.AttestationResult{}, fmt.Errorf("%w: %s attested call %q %s", domain.ErrConflict, stated.Attester.ID, key.CallID, stated.Outcome)
		default:
			agreeing++
		}
	}
	if own != nil && !sameAttestation(*own, attestation) {
		return domain.AttestationResult{}, fmt.Errorf("%w: %s already attested call %q differently", domain.ErrConflict, attestation.Attester.ID, key.CallID)
	}
	if !outcome.Status.Open() {
		if own != nil && outcome.Resolution == domain.ResolvedByAttestation && outcome.Status == attestation.Outcome {
			return domain.AttestationResult{Outcome: store.current(key), AlreadyStated: true}, nil
		}
		return domain.AttestationResult{}, fmt.Errorf("%w: call %q is already %s by %s", domain.ErrConflict, key.CallID, outcome.Status, outcome.Resolution)
	}
	if store.now().Before(outcome.DeadlineAt) {
		return domain.AttestationResult{}, fmt.Errorf("%w: call %q may still be in flight; attest it after its deadline", domain.ErrConflict, key.CallID)
	}
	if !store.unworked(key, attestation.TurnFence) {
		return domain.AttestationResult{}, fmt.Errorf("%w: turn %q is not held or ended at fence %d", domain.ErrConflict, key.RequestID, attestation.TurnFence)
	}
	if own == nil {
		if store.attestations == nil {
			store.attestations = map[domain.CallOutcomeKey][]domain.CallAttestation{}
		}
		store.attestations[key] = append(store.attestations[key], attestation)
	}
	awaits := agreeing < attestation.Persons
	if !awaits {
		outcome.Status, outcome.Resolution, outcome.Resolver = attestation.Outcome, domain.ResolvedByAttestation, attestation.Attester
		outcome.Note, outcome.Evidence, outcome.ResolvedFence, outcome.ResolvedAt = attestation.Note, attestation.Evidence, attestation.TurnFence, store.now()
	}
	return domain.AttestationResult{Outcome: store.current(key), AwaitsSecond: awaits, AlreadyStated: own != nil}, nil
}

// keep stores a result record in the object codec, or in memory without one.
func (store *MemoryCallOutcomeStore) keep(ctx context.Context, key domain.CallOutcomeKey, record domain.CallRecord) (domain.ObjectRef, error) {
	if store.Objects != nil {
		return dehydrateCallRecord(ctx, store.Objects, key, record)
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return domain.ObjectRef{}, fmt.Errorf("encode call record: %w", err)
	}
	digest := DigestBytes(payload)
	ref := domain.ObjectRef{URI: "memory://" + callRecordName(key) + "/" + digest, Digest: digest}
	if store.records == nil {
		store.records = map[string][]byte{}
	}
	store.records[ref.URI] = payload
	return ref, nil
}

// Record reads the result record of a settled or accepted call.
func (store *MemoryCallOutcomeStore) Record(ctx context.Context, ref domain.ObjectRef) (domain.CallRecord, error) {
	if store.Objects != nil {
		return HydrateCallRecord(ctx, store.Objects, ref)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	payload, found := store.records[ref.URI]
	if !found {
		return domain.CallRecord{}, fmt.Errorf("%w: call record %q", domain.ErrNotFound, ref.URI)
	}
	if digest := DigestBytes(payload); digest != ref.Digest {
		return domain.CallRecord{}, fmt.Errorf("%w: call record %q stored %s, expected %s", ErrDigestMismatch, ref.URI, digest, ref.Digest)
	}
	var record domain.CallRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return domain.CallRecord{}, fmt.Errorf("decode call record: %w", err)
	}
	return record, nil
}
