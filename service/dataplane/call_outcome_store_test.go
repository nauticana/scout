package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var outcomeOwner = domain.TurnOwner{TenantID: outcomeTurnTenant, RequestID: outcomeTurnRequest, Fence: 3}

func testCallKey(callID string) domain.CallOutcomeKey {
	return domain.CallOutcomeKey{TenantID: outcomeTurnTenant, RequestID: outcomeTurnRequest, ExecutionStepID: 11, CallID: callID}
}

func outcomeIntent(callID string) domain.CallIntent {
	return domain.CallIntent{Key: testCallKey(callID), IdempotencyKey: "loop:" + callID, Tool: domain.ToolReference{ToolID: "shop.write", Version: "2"}, Window: 90 * time.Second}
}

func newOutcomeStore(table *outcomeTable) *TableCallOutcomeStore {
	return &TableCallOutcomeStore{DB: table, Objects: newObjectStateStore(&fake.ObjectStorage{})}
}

func observationOf(callID string, entryNo int) domain.LoopEntry {
	return domain.LoopEntry{EntryNo: entryNo, Kind: domain.LoopEntryObservation, Iteration: 1,
		Observation: &domain.ModelToolObservation{CallID: callID, Name: "shop_write", Output: []byte(`{"ok":true}`)}}
}

var outcomeLoopKey = domain.LoopKey{TenantID: outcomeTurnTenant, RequestID: outcomeTurnRequest, ExecutionStepID: 11}

// §4: the intent is recorded in flight under the owner's fence before the call leaves, with its
// deadline the store clock plus the dispatch window; a repeat finds it, a key naming another call
// conflicts, and a worker the turn moved past records nothing.
func TestTableCallOutcomeStoreRecordsTheIntentBeforeTheEffect(t *testing.T) {
	ctx := context.Background()
	table := newOutcomeTable(domain.TurnRunning, 3)
	store := newOutcomeStore(table)
	outcome, created, err := store.Intend(ctx, outcomeOwner, outcomeIntent("c1"))
	if err != nil || !created || outcome.Status != domain.CallInFlight || outcome.DispatchFence != 3 || outcome.Origin != domain.CallFromAgent ||
		!outcome.DeadlineAt.Equal(table.now.Add(90*time.Second)) || outcome.DeadlinePassed {
		t.Fatalf("intent = %+v, %v, %v", outcome, created, err)
	}
	if args := table.args[qOutcomeIntend][0]; args[0] != outcomeTurnTenant || args[3] != outcomeTurnTenant || args[10] != outcomeOwner.Fence || args[11] != int64(90) {
		t.Fatalf("intent args = %v, want the owner's tenant, fence, and window", args)
	}
	again, created, err := store.Intend(ctx, outcomeOwner, outcomeIntent("c1"))
	if err != nil || created || again.Key != outcome.Key {
		t.Fatalf("repeat = %+v, %v, %v", again, created, err)
	}
	reused := outcomeIntent("c2")
	reused.IdempotencyKey = "loop:c1"
	if _, _, err := store.Intend(ctx, outcomeOwner, reused); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an idempotency key naming another call = %v, want conflict", err)
	}
	stale := outcomeOwner
	stale.Fence = 2
	if _, _, err := store.Intend(ctx, stale, outcomeIntent("c3")); !errors.Is(err, domain.ErrFenceLost) || table.statusOf(testCallKey("c3")) != "" {
		t.Fatalf("a stale owner's intent = %v", err)
	}
	other := outcomeIntent("c4")
	other.Key.TenantID = 8
	if _, _, err := store.Intend(ctx, outcomeOwner, other); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("an owner writing another tenant's call = %v, want ErrFenceLost", err)
	}
	for _, intent := range []domain.CallIntent{{Key: testCallKey("c5"), Tool: domain.ToolReference{ToolID: "t", Version: "1"}, Window: time.Second},
		{Key: testCallKey("c5"), IdempotencyKey: "k", Tool: domain.ToolReference{ToolID: "t", Version: "1"}}} {
		if _, _, err := store.Intend(ctx, outcomeOwner, intent); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("malformed intent %+v = %v", intent, err)
		}
	}
}

func TestMemoryCallOutcomeStoreWithoutFenceAuthorityFailsClosed(t *testing.T) {
	store := &MemoryCallOutcomeStore{}
	if _, _, err := store.Intend(context.Background(), outcomeOwner, outcomeIntent("c1")); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("intent without fence authority = %v, want ErrFenceLost", err)
	}
}

func TestMemoryCallOutcomeStoreVerifiesRecordDigest(t *testing.T) {
	store := &MemoryCallOutcomeStore{records: map[string][]byte{"memory://record": []byte(`{}`)}}
	if _, err := store.Record(context.Background(), domain.ObjectRef{URI: "memory://record", Digest: strings.Repeat("0", 64)}); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("record with the wrong digest = %v, want ErrDigestMismatch", err)
	}
}

// §8.15: the settlement and the observation append commit together or not at all.
func TestTableCallOutcomeStoreSettlesAndAppendsAtomically(t *testing.T) {
	ctx := context.Background()
	table := newOutcomeTable(domain.TurnRunning, 3)
	store := newOutcomeStore(table)
	if _, _, err := store.Intend(ctx, outcomeOwner, outcomeIntent("c1")); err != nil {
		t.Fatal(err)
	}
	settlement := domain.CallSettlement{Key: testCallKey("c1"), Status: domain.CallCompleted, Resolution: domain.ResolvedByGateway,
		Record: domain.CallRecord{Output: []byte(`{"ok":true}`), Usage: domain.Usage{ToolCalls: 1}}}
	table.failOn[qOutcomeJournal] = errors.New("connection reset")
	if _, err := store.SettleAndAppend(ctx, outcomeOwner, settlement, outcomeLoopKey, observationOf("c1", 2)); err == nil {
		t.Fatal("an injected journal failure must fail the settlement")
	}
	if status := table.statusOf(testCallKey("c1")); status != domain.CallInFlight || len(table.journal) != 0 || table.commits != 0 {
		t.Fatalf("after the failure: status %s, journal %v, commits %d; both must roll back", status, table.journal, table.commits)
	}
	delete(table.failOn, qOutcomeJournal)
	stored, err := store.SettleAndAppend(ctx, outcomeOwner, settlement, outcomeLoopKey, observationOf("c1", 2))
	if err != nil || stored.EntryNo != 2 || table.statusOf(testCallKey("c1")) != domain.CallCompleted || len(table.journal) != 1 || table.commits != 1 {
		t.Fatalf("settlement = %+v, %v; status %s, journal %v", stored, err, table.statusOf(testCallKey("c1")), table.journal)
	}
	if table.calls[len(table.calls)-3] != qOutcomeOwnerGuard {
		t.Fatalf("the guarded touch must open the settlement: %v", table.calls)
	}
	recorded, _, err := store.Find(ctx, testCallKey("c1"))
	if err != nil || recorded.Resolution != domain.ResolvedByGateway || recorded.ResolvedFence != 3 || recorded.Result.URI == "" {
		t.Fatalf("settled record = %+v, %v", recorded, err)
	}
	if record, err := store.Record(ctx, recorded.Result); err != nil || string(record.Output) != `{"ok":true}` {
		t.Fatalf("result object = %+v, %v", record, err)
	}
	// A repeat is accepted; a different outcome of a settled call conflicts and appends nothing.
	if _, err := store.SettleAndAppend(ctx, outcomeOwner, settlement, outcomeLoopKey, observationOf("c1", 2)); err != nil {
		t.Fatalf("a repeated settlement = %v", err)
	}
	failed := settlement
	failed.Status = domain.CallFailed
	if _, err := store.SettleAndAppend(ctx, outcomeOwner, failed, outcomeLoopKey, observationOf("c1", 3)); !errors.Is(err, domain.ErrConflict) || len(table.journal) != 1 {
		t.Fatalf("a conflicting settlement = %v, journal %v", err, table.journal)
	}
	if _, err := store.SettleAndAppend(ctx, outcomeOwner, settlement, outcomeLoopKey, observationOf("c9", 3)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an observation of another call = %v", err)
	}
}

// §8.2 against the statement shapes: once a hold moved the fence, the old owner's intent, unknown
// marking, and settlement are refused, so nothing is added after the hold; its late report settles
// only its own dispatch, as evidence. A call that never left is retracted by its dispatch fence only.
func TestTableCallOutcomeStoreFencesOutAHeldTurnsWorker(t *testing.T) {
	ctx := context.Background()
	table := newOutcomeTable(domain.TurnRunning, 3)
	store := newOutcomeStore(table)
	if _, _, err := store.Intend(ctx, outcomeOwner, outcomeIntent("c1")); err != nil {
		t.Fatal(err)
	}
	table.status, table.fence = domain.TurnHeld, 4
	if _, _, err := store.Intend(ctx, outcomeOwner, outcomeIntent("c2")); !errors.Is(err, domain.ErrFenceLost) || table.statusOf(testCallKey("c2")) != "" {
		t.Fatalf("an intent after the hold = %v", err)
	}
	if err := store.MarkUnknown(ctx, outcomeOwner, testCallKey("c1")); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("marking after the hold = %v", err)
	}
	settlement := domain.CallSettlement{Key: testCallKey("c1"), Status: domain.CallCompleted, Resolution: domain.ResolvedByGateway,
		Record: domain.CallRecord{Output: []byte(`{}`)}}
	if _, err := store.SettleAndAppend(ctx, outcomeOwner, settlement, outcomeLoopKey, observationOf("c1", 2)); !errors.Is(err, domain.ErrFenceLost) || len(table.journal) != 0 {
		t.Fatalf("a settlement after the hold = %v, journal %v", err, table.journal)
	}
	late := settlement
	late.Resolution = domain.ResolvedByWorker
	if err := store.Report(ctx, late, 2); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a report under another dispatch fence = %v", err)
	}
	if err := store.Report(ctx, late, 3); err != nil {
		t.Fatalf("the worker's own late report = %v", err)
	}
	reported, _, _ := store.Find(ctx, testCallKey("c1"))
	if reported.Status != domain.CallCompleted || reported.Resolution != domain.ResolvedByWorker || reported.ResolvedFence != 3 || len(table.journal) != 0 {
		t.Fatalf("late report = %+v, journal %v; evidence only", reported, table.journal)
	}
	if err := store.Report(ctx, settlement, 3); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a report must be the worker's: %v", err)
	}

	table.status, table.fence = domain.TurnRunning, 5
	owner := outcomeOwner
	owner.Fence = 5
	if _, _, err := store.Intend(ctx, owner, outcomeIntent("c3")); err != nil {
		t.Fatal(err)
	}
	if err := store.Retract(ctx, testCallKey("c3"), 4); err != nil || table.statusOf(testCallKey("c3")) != domain.CallInFlight {
		t.Fatalf("a retraction under another dispatch fence removed the record: %v", err)
	}
	if err := store.Retract(ctx, testCallKey("c3"), 5); err != nil || table.statusOf(testCallKey("c3")) != "" {
		t.Fatalf("retraction = %v, status %s", err, table.statusOf(testCallKey("c3")))
	}
	if err := store.MarkUnknown(ctx, owner, testCallKey("c1")); err != nil || table.statusOf(testCallKey("c1")) != domain.CallCompleted {
		t.Fatalf("marking a settled call unknown must change nothing: %v", err)
	}
}

func holdOutcome(t *testing.T, table *outcomeTable, store *TableCallOutcomeStore, callID string, heldFence int64) {
	t.Helper()
	if _, _, err := store.Intend(context.Background(), outcomeOwner, outcomeIntent(callID)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnknown(context.Background(), outcomeOwner, testCallKey(callID)); err != nil {
		t.Fatal(err)
	}
	table.status, table.fence = domain.TurnHeld, heldFence
}

func attestation(callID, person string, outcome domain.CallOutcomeStatus, note string, persons int) domain.CallAttestation {
	return domain.CallAttestation{Key: testCallKey(callID), Attester: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: person},
		Outcome: outcome, Note: note, Persons: persons, TurnFence: 4}
}

// §8.6: attestation waits for the deadline by the store clock and a held or ended turn at the
// fence the person saw; an identical repeat is a no-op and a conflicting one is refused.
func TestTableCallOutcomeStoreAttestsOnlyAfterTheDeadline(t *testing.T) {
	ctx := context.Background()
	table := newOutcomeTable(domain.TurnRunning, 3)
	store := newOutcomeStore(table)
	holdOutcome(t, table, store, "c1", 4)
	if _, err := store.Attest(ctx, attestation("c1", "9", domain.CallCompleted, "shipped per ERP", 1)); !errors.Is(err, domain.ErrConflict) ||
		!strings.Contains(err.Error(), "deadline") || table.statusOf(testCallKey("c1")) != domain.CallUnknown {
		t.Fatalf("an attestation before the deadline = %v", err)
	}
	table.now = table.now.Add(91 * time.Second)
	stale := attestation("c1", "9", domain.CallCompleted, "shipped per ERP", 1)
	stale.TurnFence = 3
	if _, err := store.Attest(ctx, stale); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("an attestation at a stale fence = %v", err)
	}
	result, err := store.Attest(ctx, attestation("c1", "9", domain.CallCompleted, "shipped per ERP", 1))
	if err != nil || result.AwaitsSecond || result.Outcome.Status != domain.CallCompleted || result.Outcome.Resolution != domain.ResolvedByAttestation ||
		result.Outcome.Resolver.ID != "9" || result.Outcome.Note != "shipped per ERP" || result.Outcome.ResolvedFence != 4 {
		t.Fatalf("attestation = %+v, %v", result, err)
	}
	replay, err := store.Attest(ctx, attestation("c1", "9", domain.CallCompleted, "shipped per ERP", 1))
	if err != nil || !replay.AlreadyStated {
		t.Fatalf("an identical replay = %+v, %v", replay, err)
	}
	for name, conflicting := range map[string]domain.CallAttestation{
		"other outcome": attestation("c1", "9", domain.CallFailed, "shipped per ERP", 1),
		"other note":    attestation("c1", "9", domain.CallCompleted, "different", 1),
		"other person":  attestation("c1", "10", domain.CallCompleted, "shipped per ERP", 1),
	} {
		if _, err := store.Attest(ctx, conflicting); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("%s replay = %v, want conflict", name, err)
		}
	}
	if _, err := store.Attest(ctx, attestation("c1", "9", domain.CallUnknown, "x", 1)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an attested outcome is completed or failed: %v", err)
	}
	if _, err := store.Attest(ctx, attestation("c1", "9", domain.CallCompleted, " ", 1)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a note is mandatory: %v", err)
	}
}

// Decision 7: a call whose attestation needs two people settles on the second agreeing person,
// never on one person twice; a second person who disagrees is refused.
func TestTableCallOutcomeStoreSettlesATwoPersonAttestationOnTheSecondPerson(t *testing.T) {
	ctx := context.Background()
	table := newOutcomeTable(domain.TurnRunning, 3)
	store := newOutcomeStore(table)
	holdOutcome(t, table, store, "c1", 4)
	table.now = table.now.Add(time.Hour)
	first, err := store.Attest(ctx, attestation("c1", "9", domain.CallFailed, "refund never posted", 2))
	if err != nil || !first.AwaitsSecond || first.Outcome.Status != domain.CallUnknown {
		t.Fatalf("first person = %+v, %v", first, err)
	}
	again, err := store.Attest(ctx, attestation("c1", "9", domain.CallFailed, "refund never posted", 2))
	if err != nil || !again.AwaitsSecond || !again.AlreadyStated || again.Outcome.Status != domain.CallUnknown {
		t.Fatalf("the same person again = %+v, %v", again, err)
	}
	if _, err := store.Attest(ctx, attestation("c1", "10", domain.CallCompleted, "it posted", 2)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a disagreeing second person = %v", err)
	}
	second, err := store.Attest(ctx, attestation("c1", "10", domain.CallFailed, "confirmed with bank", 2))
	if err != nil || second.AwaitsSecond || second.Outcome.Status != domain.CallFailed || second.Outcome.Resolver.ID != "10" {
		t.Fatalf("second person = %+v, %v", second, err)
	}
	if len(table.attests[testCallKey("c1")]) != 2 {
		t.Fatalf("each person's attestation is kept: %+v", table.attests)
	}
}

// A person's lookup settles an open call only on a turn held or ended at the fence they saw.
func TestTableCallOutcomeStoreResolvesOnlyAnUnworkedTurn(t *testing.T) {
	ctx := context.Background()
	table := newOutcomeTable(domain.TurnRunning, 3)
	store := newOutcomeStore(table)
	holdOutcome(t, table, store, "c1", 4)
	settlement := domain.CallSettlement{Key: testCallKey("c1"), Status: domain.CallCompleted, Resolution: domain.ResolvedByLookup,
		Record: domain.CallRecord{Output: []byte(`{"refund":"r-1"}`)}}
	person := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}
	if _, err := store.Resolve(ctx, settlement, person, 3); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a stale fence = %v", err)
	}
	resolved, err := store.Resolve(ctx, settlement, person, 4)
	if err != nil || resolved.Status != domain.CallCompleted || resolved.Resolution != domain.ResolvedByLookup || resolved.Resolver != person {
		t.Fatalf("resolution = %+v, %v", resolved, err)
	}
	if _, err := store.Resolve(ctx, settlement, person, 4); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("a settled call = %v", err)
	}
	attested := settlement
	attested.Resolution = domain.ResolvedByAttestation
	if _, err := store.Resolve(ctx, attested, person, 4); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an attestation is not a lookup: %v", err)
	}
}

// The MCP confirmation hook settles, in the reconciling transaction, only the calls linked to that
// confirmation in its own tenant.
func TestTableCallOutcomeStoreSettlesTheCallsAReconciledConfirmationLinks(t *testing.T) {
	ctx := context.Background()
	table := newOutcomeTable(domain.TurnRunning, 3)
	store := newOutcomeStore(table)
	if _, _, err := store.Intend(ctx, outcomeOwner, outcomeIntent("c1")); err != nil {
		t.Fatal(err)
	}
	accepted, err := store.Accept(ctx, outcomeOwner, testCallKey("c1"),
		domain.CallRecord{Acceptance: &domain.ToolAcceptance{Handle: "conf-41", LookupWithin: time.Hour, MCPConfirmationID: 41}}, time.Hour+time.Minute)
	if err != nil || accepted.Status != domain.CallAccepted || !accepted.DeadlineAt.Equal(table.now.Add(time.Hour+time.Minute)) || table.links[testCallKey("c1")] != 41 {
		t.Fatalf("acceptance = %+v, %v, links %v", accepted, err, table.links)
	}
	if args := table.args[qOutcomeLink][0]; args[0] != outcomeTurnTenant || args[4] != int64(41) {
		t.Fatalf("link args = %v, want the call's tenant", args)
	}
	reconciler := domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}
	if err := store.ConfirmationReconciledTx(ctx, table, domain.MCPConfirmationKey{TenantID: 8, ID: 41}, domain.MCPConfirmationExecuted, reconciler); err != nil {
		t.Fatal(err)
	}
	if table.statusOf(testCallKey("c1")) != domain.CallAccepted {
		t.Fatal("another tenant's confirmation settled the call")
	}
	if err := store.ConfirmationReconciledTx(ctx, table, domain.MCPConfirmationKey{TenantID: 7, ID: 41}, domain.MCPConfirmationExecuted, reconciler); err != nil {
		t.Fatal(err)
	}
	settled, _, _ := store.Find(ctx, testCallKey("c1"))
	if settled.Status != domain.CallCompleted || settled.Resolution != domain.ResolvedByConfirmation || settled.Resolver != reconciler {
		t.Fatalf("settled = %+v", settled)
	}
	if err := store.ConfirmationReconciledTx(ctx, table, domain.MCPConfirmationKey{TenantID: 7, ID: 41}, domain.MCPConfirmationPending, reconciler); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a confirmation not reconciled = %v", err)
	}
	if err := store.ConfirmationReconciledTx(ctx, &persistenceQueryFakeWithoutCatalog{}, domain.MCPConfirmationKey{TenantID: 7, ID: 41}, domain.MCPConfirmationFailed, reconciler); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a transaction without a catalog = %v", err)
	}
	if _, err := store.Accept(ctx, outcomeOwner, testCallKey("c1"), domain.CallRecord{}, time.Minute); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("an acceptance without its handle = %v", err)
	}
}

// persistenceQueryFakeWithoutCatalog is a transaction that cannot bind another query catalog.
type persistenceQueryFakeWithoutCatalog struct{ queueQueryFake }

// Every statement of the store names the row's tenant; a call of another tenant is never found.
func TestTableCallOutcomeStoreScopesEveryStatementToItsTenant(t *testing.T) {
	for name, sql := range callOutcomeQueries {
		if !strings.Contains(sql, "partner_id = ?") && !strings.Contains(sql, "(partner_id, request_id") {
			t.Errorf("%s does not bind a tenant: %s", name, sql)
		}
	}
	ctx := context.Background()
	table := newOutcomeTable(domain.TurnRunning, 3)
	store := newOutcomeStore(table)
	if _, _, err := store.Intend(ctx, outcomeOwner, outcomeIntent("c1")); err != nil {
		t.Fatal(err)
	}
	foreign := testCallKey("c1")
	foreign.TenantID = 8
	if _, found, err := store.Find(ctx, foreign); err != nil || found {
		t.Fatalf("another tenant read the call: %v, %v", found, err)
	}
	if listed, err := store.List(ctx, 8, outcomeTurnRequest); err != nil || len(listed) != 0 {
		t.Fatalf("another tenant listed %v, %v", listed, err)
	}
	if listed, err := store.List(ctx, outcomeTurnTenant, outcomeTurnRequest); err != nil || len(listed) != 1 {
		t.Fatalf("the tenant lists %v, %v", listed, err)
	}
	if err := store.Retract(ctx, foreign, 3); err != nil || table.statusOf(testCallKey("c1")) != domain.CallInFlight {
		t.Fatalf("another tenant retracted the call: %v", err)
	}
}
