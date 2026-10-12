package dataplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/isolation"
)

// memoryLedger is one turn's reservation with the ledger's semantics: a held reservation replays,
// a settled one is ErrBudgetSettled, and a settlement moves it held to settled once.
type memoryLedger struct {
	mu       sync.Mutex
	status   domain.ReservationStatus
	settled  []domain.Usage
	reserved int
}

func (ledger *memoryLedger) Reserve(_ context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.reserved++
	reservation := domain.BudgetReservation{TenantID: request.TenantID, ReservationID: "reservation-1", RequestID: request.RequestID,
		Attempt: 1, GrantedTokens: request.Tokens, Currency: request.Currency}
	switch ledger.status {
	case "":
		ledger.status = domain.ReservationHeld
	case domain.ReservationHeld:
		reservation.Replayed = true
	case domain.ReservationSettled:
		return domain.BudgetReservation{}, fmt.Errorf("%w: request %q", domain.ErrBudgetSettled, request.RequestID)
	}
	return reservation, nil
}

func (ledger *memoryLedger) settle(usage domain.Usage) error {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.status != domain.ReservationHeld {
		return fmt.Errorf("%w: reservation is %s", domain.ErrConflict, ledger.status)
	}
	ledger.status, ledger.settled = domain.ReservationSettled, append(ledger.settled, usage)
	return nil
}

// Decision 18 regression: a fence that moves between settling the reservation and completing the
// turn leaves the reservation held, so the run that resumes the turn holds a live reservation and
// settles it once. On the earlier two-transaction path the first settlement committed, the
// completion was refused, and the resumed run found the reservation settled and ran unheld.
func TestTurnRuntimeNeverSettlesAReservationWhoseCompletionIsRefused(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	ledger := &memoryLedger{}
	runtime.Budget = &fake.TenantBudgetManager{ReserveFunc: ledger.Reserve}
	fences := &MemoryTurnFences{}
	runtime.Records.(*fake.TurnRecordStore).AcquireFunc = func(context.Context, domain.TurnClaim) (domain.TurnOwner, error) {
		return fences.Acquire(7, "request-1")
	}
	var unheld []string
	sessions := runtime.Sessions.(*fake.SessionCoordinator)
	sessions.CompleteFunc = func(_ context.Context, owner domain.TurnOwner, _ string, _ int64, _ domain.TurnResult, settlement domain.TurnSettlement) error {
		if owner.Fence == 1 {
			// The fence moves under the completing worker: a lease reclaimed, or a preempt.
			fences.Hold(7, "request-1")
		}
		if err := fences.Check(owner); err != nil {
			return err
		}
		if settlement.Reservation.ReservationID == "" {
			unheld = append(unheld, owner.RequestID)
			return nil
		}
		recorder.record("complete")
		return ledger.settle(settlement.Usage)
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("the fenced-out completion = %v, want ErrFenceLost", err)
	}
	if ledger.status != domain.ReservationHeld || len(ledger.settled) != 0 {
		t.Fatalf("a refused completion settled the reservation: %s, %v", ledger.status, ledger.settled)
	}
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	if len(unheld) != 0 || ledger.status != domain.ReservationSettled || len(ledger.settled) != 1 || ledger.reserved != 2 {
		t.Fatalf("the resumed run must complete on its live reservation: unheld %v, status %s, settled %v, reserved %d",
			unheld, ledger.status, ledger.settled, ledger.reserved)
	}
}

// budgetRow is a stored reservation as the ledger's lookup returns it.
func budgetRow(status domain.ReservationStatus, expired bool) []any {
	return []any{"reservation-1", "req-1", string(status), int64(100), int64(10), "USD", time.Now().Add(time.Minute), int64(0), int64(0), int64(1), expired}
}

func settlingSessionStore(query *persistenceQueryFake) *DurableSessionStore {
	store := newDurableSessionStore(query, &fake.ObjectStorage{})
	store.Settlement.Budget = &isolation.BudgetLedger{}
	return store
}

// Decision 18 at the store: the guarded touch opens one transaction in which the reservation is
// settled under the tenant lock, the usage events are written, and the turn completes; nothing
// commits when the completion is refused, and a reservation no longer held refuses the completion.
func TestDurableSessionStoreSettlesAndCompletesInOneTransaction(t *testing.T) {
	ctx := context.Background()
	response := domain.TurnResult{Response: []byte(`{"text":"done"}`)}
	query := &persistenceQueryFake{rows: map[string][][]any{
		qSessionActiveTurn: {{int64(4), int64(6), "req-1"}}, qSessionCompleteTurn: {{int64(4)}},
		"scout_isolation_get_budget": {budgetRow(domain.ReservationHeld, false)}, "scout_isolation_settle_budget": {{"reservation-1"}},
		qLedgerInsertUsageEvent: {{int64(1)}},
	}}
	if err := settlingSessionStore(query).Complete(ctx, sessionOwner, "conv", 6, response, testSettlement); err != nil {
		t.Fatal(err)
	}
	want := []string{qSessionActiveTurn, qSessionOwnerGuard, "scout_isolation_lock_budget", "scout_isolation_get_budget",
		"scout_isolation_settle_budget", qLedgerUsageSettled, qLedgerInsertUsageEvent, qSessionCompleteTurn}
	if !slices.Equal(query.calls, want) || query.commits != 1 {
		t.Fatalf("statements %v, commits %d; want %v in one transaction", query.calls, query.commits, want)
	}
	if args := query.lastArgs("scout_isolation_settle_budget"); args[0] != int64(5) || args[1] != int64(2) || args[2] != int64(7) {
		t.Fatalf("settled at %v, want the turn's usage in its tenant", args)
	}

	refused := &persistenceQueryFake{rows: map[string][][]any{
		qSessionActiveTurn: {{int64(4), int64(6), "req-1"}}, qSessionTurnFence: {{int64(4), string(domain.TurnHeld)}},
		"scout_isolation_get_budget": {budgetRow(domain.ReservationHeld, false)}, "scout_isolation_settle_budget": {{"reservation-1"}},
	}}
	if err := settlingSessionStore(refused).Complete(ctx, sessionOwner, "conv", 6, response, testSettlement); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("a refused completion = %v, want ErrFenceLost", err)
	}
	if refused.commits != 0 || refused.rollbacks != 1 {
		t.Fatalf("a refused completion committed its settlement: commits %d, rollbacks %d", refused.commits, refused.rollbacks)
	}

	lapsed := &persistenceQueryFake{rows: map[string][][]any{
		qSessionActiveTurn: {{int64(4), int64(6), "req-1"}}, qSessionCompleteTurn: {{int64(4)}},
		"scout_isolation_get_budget": {budgetRow(domain.ReservationHeld, true)}, "scout_isolation_expire_one_budget": {{"reservation-1"}},
	}}
	if err := settlingSessionStore(lapsed).Complete(ctx, sessionOwner, "conv", 6, response, testSettlement); !errors.Is(err, domain.ErrConflict) ||
		slices.Contains(lapsed.calls, qSessionCompleteTurn) || lapsed.commits != 0 {
		t.Fatalf("a lapsed reservation = %v, calls %v, commits %d", err, lapsed.calls, lapsed.commits)
	}

	fenced := &persistenceQueryFake{rows: map[string][][]any{qSessionActiveTurn: {{int64(4), int64(6), "req-1"}}, qSessionOwnerGuard: nil}}
	store := settlingSessionStore(fenced)
	fenced.rows[qSessionOwnerGuard] = nil
	if err := store.Complete(ctx, sessionOwner, "conv", 6, response, testSettlement); !errors.Is(err, domain.ErrFenceLost) ||
		slices.Contains(fenced.calls, "scout_isolation_lock_budget") {
		t.Fatalf("a fenced-out owner = %v, calls %v; it must settle nothing", err, fenced.calls)
	}
	if err := (&DurableSessionStore{DB: persistenceDBFake{query: &persistenceQueryFake{}}, Objects: newObjectStateStore(&fake.ObjectStorage{})}).
		Complete(ctx, sessionOwner, "conv", 6, response, testSettlement); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a store that cannot settle = %v, want validation", err)
	}
}

// A failing turn's end commits with its settlement; a reservation no longer held does not keep it
// from failing, and says so. A fenced-out owner fails and settles nothing.
func TestTableTurnRecordStoreFailsWithItsSettlement(t *testing.T) {
	ctx := context.Background()
	settler := &settlerFake{err: fmt.Errorf("%w: reservation lapsed", domain.ErrConflict)}
	query := &queueQueryFake{rows: map[string][][]any{qRecordFail: {{int64(4)}}}}
	store := newTestRecordStore(query)
	store.Settlement.Budget = settler
	err := store.Fail(ctx, recordOwner, domain.TurnFailed, "model_error", testSettlement)
	if !errors.Is(err, domain.ErrBudgetUnsettled) || !slices.Contains(query.calls, qRecordFail) || query.calls[0] != qRecordOwnerTurn {
		t.Fatalf("fail with a lapsed reservation = %v, calls %v", err, query.calls)
	}
	refund := domain.TurnSettlement{Reservation: testSettlement.Reservation, Refund: true}
	refunding := &settlerFake{}
	store.Settlement.Budget = refunding
	query.calls = nil
	if err := store.Fail(ctx, recordOwner, domain.TurnCancelled, "canceled", refund); err != nil || refunding.calls[0] != "res-1/0/true" ||
		slices.Contains(query.calls, qLedgerInsertUsageEvent) {
		t.Fatalf("refund = %v, settlements %v, calls %v", err, refunding.calls, query.calls)
	}
	fenced := &queueQueryFake{rows: map[string][][]any{qRecordOwnerTurn: nil}}
	fencedStore := newTestRecordStore(fenced)
	fenced.rows[qRecordOwnerTurn] = nil
	untouched := &settlerFake{}
	fencedStore.Settlement.Budget = untouched
	if err := fencedStore.Fail(ctx, recordOwner, domain.TurnFailed, "late", testSettlement); !errors.Is(err, domain.ErrFenceLost) || len(untouched.calls) != 0 {
		t.Fatalf("a fenced-out failure = %v, settlements %v", err, untouched.calls)
	}
}
