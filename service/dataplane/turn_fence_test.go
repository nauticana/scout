package dataplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/dataplane/dataplanetest"
)

// fenceTable is one conversation_turn row and the lease of its turn_queue row, answering the
// fenced statements by their SQL semantics: a guarded touch, or a write carrying one, matches
// only while the owner's fence and a worker-owned status hold, and an acquisition only for the
// current lease. It stands in for PostgreSQL's row-level linearization, which it serializes.
type fenceTable struct {
	keelport.DatabaseRepository
	mu      sync.Mutex
	status  domain.TurnStatus
	fence   int64
	leaseID int64
	token   int64
	worker  string
	calls   []string
}

const (
	fenceTenant  = int64(7)
	fenceRequest = "request-1"
	fenceTurnNo  = int64(4)
)

func newFenceTable() *fenceTable {
	return &fenceTable{status: domain.TurnQueued, leaseID: 11, token: 99, worker: "worker-1"}
}

func (table *fenceTable) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return table
}
func (table *fenceTable) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return table, nil
}
func (*fenceTable) GenID() int64                   { return 0 }
func (*fenceTable) Commit(context.Context) error   { return nil }
func (*fenceTable) Rollback(context.Context) error { return nil }

func (table *fenceTable) owned(tenantID, requestID, fence any) bool {
	return tenantID == fenceTenant && requestID == fenceRequest && fence == table.fence &&
		(table.status == domain.TurnRunning || table.status == domain.TurnStreaming)
}

func (table *fenceTable) currentLease(id, token, worker any) bool {
	return id == table.leaseID && token == table.token && worker == table.worker
}

func rows(values ...[]any) *keelmodel.QueryResult { return &keelmodel.QueryResult{Rows: values} }

func (table *fenceTable) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	table.calls = append(table.calls, name)
	fenced := func(written bool) *keelmodel.QueryResult {
		if !table.owned(args[0], args[1], args[2]) {
			return rows([]any{int64(0), int64(0)})
		}
		if !written {
			return rows([]any{int64(1), int64(0)})
		}
		return rows([]any{int64(1), int64(1)})
	}
	switch name {
	case qRecordAcquire:
		live := table.status == domain.TurnQueued || table.status == domain.TurnRunning || table.status == domain.TurnStreaming
		if args[0] != fenceTenant || args[1] != fenceRequest || !live || !table.currentLease(args[2], args[3], args[4]) {
			return rows(), nil
		}
		table.status, table.fence = domain.TurnRunning, table.fence+1
		return rows([]any{fenceTurnNo, table.fence}), nil
	case qRecordAcquireProbe:
		return rows([]any{string(table.status), table.currentLease(args[0], args[1], args[2])}), nil
	case qRecordFind:
		return rows([]any{fenceTurnNo, "conversation-1", string(table.status), testDigest("input"), nil, nil, table.fence, ""}), nil
	case qRecordOwnerState:
		return rows([]any{table.fence, string(table.status), false, ""}), nil
	case qRecordOwnerGuard, qSessionOwnerGuard, qOutcomeOwnerGuard:
		if !table.owned(args[0], args[1], args[2]) {
			return rows(), nil
		}
		return rows([]any{fenceTurnNo}), nil
	case qRecordOwnerTurn:
		if !table.owned(args[0], args[1], args[2]) {
			return rows(), nil
		}
		return rows([]any{fenceTurnNo, "conversation-1"}), nil
	case qRecordHold, qRecordSuspend:
		if name == qRecordHold {
			args = args[1:]
		}
		if !table.owned(args[0], args[1], args[2]) {
			return rows(), nil
		}
		table.status, table.fence = map[string]domain.TurnStatus{qRecordHold: domain.TurnHeld, qRecordSuspend: domain.TurnSuspended}[name], table.fence+1
		return rows([]any{table.fence}), nil
	case qRecordFail:
		if !table.owned(args[3], args[4], args[5]) {
			return rows(), nil
		}
		table.status = domain.TurnStatus(args[0].(string))
		return rows([]any{fenceTurnNo}), nil
	case qLoopEntryInsert, qStepClaim, qOutcomeIntend, qOutcomeMarkUnknown, qOutcomeRedispatch:
		return fenced(true), nil
	case qOutcomeFind:
		return rows([]any{fenceRequest, args[2], args[3], "loop:c1", "shop.write", "2", string(domain.CallFromAgent), string(domain.CallInFlight),
			table.fence, time.Now(), time.Now().Add(time.Minute), false, "", "", "", int64(0), "", "", nil, "", "", ""}), nil
	case qOutcomeSettle:
		return rows([]any{"c1"}), nil
	case qOutcomeJournal:
		return rows([]any{int64(1)}), nil
	case qStepCommit, qStepAbandon, qStepReplay, qStepReclaimExpired:
		return fenced(true), nil
	case qSessionCreateSnapshot, qSessionAdvanceSnapshot:
		return rows([]any{int64(1)}), nil
	case qSessionActiveTurn:
		if table.status.Terminal() {
			return rows(), nil
		}
		return rows([]any{fenceTurnNo, int64(0), fenceRequest}), nil
	case qSessionCompleteTurn:
		if args[5] != fenceRequest || !table.owned(fenceTenant, args[5], args[6]) {
			return rows(), nil
		}
		table.status = domain.TurnCompleted
		return rows([]any{fenceTurnNo}), nil
	case qSessionTurnFence:
		return rows([]any{table.fence, string(table.status)}), nil
	case qLedgerInsertUsageEvent:
		return rows([]any{int64(1)}), nil
	}
	return rows(), nil
}

// acquire takes the turn with its current lease, as RuntimeWorker's delivery does.
func (table *fenceTable) acquire(t *testing.T, store *TableTurnRecordStore) domain.TurnOwner {
	t.Helper()
	claim := claimOf(fenceDispatch())
	table.mu.Lock()
	claim.LeaseToken, claim.WorkerID = table.token, table.worker
	table.mu.Unlock()
	owner, err := store.Acquire(context.Background(), claim)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return owner
}

// redeliver expires the lease and hands the row to a new delivery, as the reclaim and a new claim do.
func (table *fenceTable) redeliver(token int64, worker string) {
	table.mu.Lock()
	defer table.mu.Unlock()
	table.token, table.worker = token, worker
}

func fenceDispatch() domain.TurnDispatch {
	dispatch := runtimeDispatch()
	dispatch.Turn.TenantContext.TenantID, dispatch.Turn.RequestID = fenceTenant, fenceRequest
	return dispatch
}

func newFenceRecordStore(table *fenceTable) *TableTurnRecordStore {
	return &TableTurnRecordStore{DB: table, Objects: newObjectStateStore(&fake.ObjectStorage{}),
		Settlement: TurnSettlementWriter{Budget: &settlerFake{}, UsageCategory: "model_output", OperatorUsageCategory: "operator"}}
}

func TestTableStoresConformToTheFenceSuite(t *testing.T) {
	dataplanetest.RunFenceSuite(t, func(t *testing.T) dataplanetest.FenceHarness {
		table := newFenceTable()
		records := newFenceRecordStore(table)
		objects := newObjectStateStore(&fake.ObjectStorage{})
		journal := &TableLoopJournal{DB: table, Objects: objects}
		outcomes := &TableCallOutcomeStore{DB: table, Objects: objects}
		steps := &StepIdempotencyStore{DB: table, Objects: objects, ClaimLease: stepLease}
		sessions := &DurableSessionStore{DB: table, Objects: objects, Settlement: TurnSettlementWriter{Budget: &settlerFake{}, UsageCategory: "model_output", OperatorUsageCategory: "operator"}}
		key := domain.LoopKey{TenantID: fenceTenant, RequestID: fenceRequest, ExecutionStepID: 11}
		entry := domain.LoopEntry{EntryNo: 1, Kind: domain.LoopEntryModel, Iteration: 1, Model: &domain.ModelResult{Output: []byte("x")}}
		checkpoint := validCheckpoint("x")
		checkpoint.TurnNo, checkpoint.ConversationID = fenceTurnNo, "conversation-1"
		settlement := domain.TurnSettlement{Reservation: domain.BudgetReservation{TenantID: fenceTenant, ReservationID: "r", RequestID: fenceRequest},
			Usage: domain.Usage{OutputTokens: 1, Currency: "USD"}, SubjectRef: "agent@v1"}
		return dataplanetest.FenceHarness{
			Acquire: func(t *testing.T) domain.TurnOwner {
				// Each acquisition comes from a newer delivery of the same queue row.
				table.redeliver(table.token+1, "worker-1")
				return table.acquire(t, records)
			},
			Hold: func(t *testing.T, owner domain.TurnOwner) {
				if _, err := records.Hold(context.Background(), owner, domain.HoldCheckpoint); err != nil {
					t.Fatalf("hold: %v", err)
				}
			},
			Writes: map[string]func(context.Context, domain.TurnOwner) error{
				"loop journal append": func(ctx context.Context, owner domain.TurnOwner) error {
					_, err := journal.Append(ctx, owner, key, entry)
					return err
				},
				"call intent and outcome": func(ctx context.Context, owner domain.TurnOwner) error {
					return fencedOutcomeWrites(ctx, outcomes, owner)
				},
				"step claim": func(ctx context.Context, owner domain.TurnOwner) error {
					_, _, err := steps.Begin(ctx, owner, testStep)
					return err
				},
				"step commit": func(ctx context.Context, owner domain.TurnOwner) error {
					return steps.Commit(ctx, owner, testStep, domain.StepResult{State: []byte("s")})
				},
				"step abandon": func(ctx context.Context, owner domain.TurnOwner) error {
					return steps.Abandon(ctx, owner, testStep)
				},
				"session checkpoint": func(ctx context.Context, owner domain.TurnOwner) error {
					return sessions.Checkpoint(ctx, owner, 0, checkpoint)
				},
				"session complete": func(ctx context.Context, owner domain.TurnOwner) error {
					return sessions.Complete(ctx, owner, "conversation-1", 0, domain.TurnResult{Response: []byte("done")}, settlement)
				},
				"turn fail": func(ctx context.Context, owner domain.TurnOwner) error {
					return records.Fail(ctx, owner, domain.TurnFailed, "model_error", settlement)
				},
				"turn suspend": func(ctx context.Context, owner domain.TurnOwner) error {
					_, err := records.Suspend(ctx, owner, "approval_pending")
					return err
				},
				"turn hold": func(ctx context.Context, owner domain.TurnOwner) error {
					_, err := records.Hold(ctx, owner, domain.HoldCheckpoint)
					return err
				},
			},
		}
	})
}

func TestMemoryStoresConformToTheFenceSuite(t *testing.T) {
	dataplanetest.RunFenceSuite(t, func(t *testing.T) dataplanetest.FenceHarness {
		steps := newMemoryIdempotency()
		journal := &MemoryLoopJournal{Fences: steps.fences}
		key := domain.LoopKey{TenantID: fenceTenant, RequestID: fenceRequest, ExecutionStepID: 11}
		return dataplanetest.FenceHarness{
			Acquire: func(t *testing.T) domain.TurnOwner {
				owner, err := steps.fences.Acquire(fenceTenant, fenceRequest)
				if err != nil {
					t.Fatal(err)
				}
				return owner
			},
			Hold: func(_ *testing.T, owner domain.TurnOwner) { steps.fences.Hold(owner.TenantID, owner.RequestID) },
			Writes: map[string]func(context.Context, domain.TurnOwner) error{
				"loop journal append": func(ctx context.Context, owner domain.TurnOwner) error {
					_, err := journal.Append(ctx, owner, key, domain.LoopEntry{EntryNo: 1, Kind: domain.LoopEntryModel})
					return err
				},
				"call intent and outcome": func(ctx context.Context, owner domain.TurnOwner) error {
					return fencedOutcomeWrites(ctx, &MemoryCallOutcomeStore{Fences: steps.fences, Journal: journal}, owner)
				},
				"step claim and commit": func(ctx context.Context, owner domain.TurnOwner) error {
					if _, _, err := steps.Begin(ctx, owner, testStep); err != nil {
						return err
					}
					return steps.Commit(ctx, owner, testStep, domain.StepResult{State: []byte("s")})
				},
			},
		}
	})
	if err := (&MemoryLoopJournal{}).Fences.Check(loopOwner); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("a memory journal without a fence authority must refuse every write, got %v", err)
	}
}

// fencedOutcomeWrites makes every owner write of a call outcome store in turn: intent, unknown
// marking, redispatch, and the settlement with its observation.
func fencedOutcomeWrites(ctx context.Context, outcomes contract.CallOutcomeStore, owner domain.TurnOwner) error {
	key := domain.CallOutcomeKey{TenantID: fenceTenant, RequestID: fenceRequest, ExecutionStepID: 11, CallID: "c1"}
	if _, _, err := outcomes.Intend(ctx, owner, domain.CallIntent{Key: key, IdempotencyKey: "loop:c1",
		Tool: domain.ToolReference{ToolID: "shop.write", Version: "2"}, Window: time.Minute}); err != nil {
		return err
	}
	if err := outcomes.MarkUnknown(ctx, owner, key); err != nil {
		return err
	}
	if _, err := outcomes.Redispatch(ctx, owner, key, time.Minute); err != nil {
		return err
	}
	entry := domain.LoopEntry{EntryNo: 1, Kind: domain.LoopEntryObservation, Observation: &domain.ModelToolObservation{CallID: "c1", Name: "shop_write"}}
	_, err := outcomes.SettleAndAppend(ctx, owner, domain.CallSettlement{Key: key, Status: domain.CallCompleted, Resolution: domain.ResolvedByGateway},
		domain.LoopKey{TenantID: fenceTenant, RequestID: fenceRequest, ExecutionStepID: 11}, entry)
	return err
}

// G1: a turn whose worker died while running is redelivered under a new lease and acquired
// again with its fence incremented, so it resumes instead of failing; the dead worker's writes
// are refused. Before the fence, Start moved only queued turns and the redelivery failed the turn.
func TestTableTurnRecordStoreReacquiresARunningTurnForItsCurrentLease(t *testing.T) {
	table := newFenceTable()
	records := newFenceRecordStore(table)
	zombie := table.acquire(t, records)
	if zombie.Fence != 1 || table.status != domain.TurnRunning {
		t.Fatalf("first acquisition: %+v, %s", zombie, table.status)
	}
	table.redeliver(100, "worker-2")
	claim := claimOf(fenceDispatch())
	claim.LeaseToken, claim.WorkerID = 100, "worker-2"
	current, err := records.Acquire(context.Background(), claim)
	if err != nil || current.Fence != 2 || table.status != domain.TurnRunning {
		t.Fatalf("redelivery = %+v, %v, status %s", current, err, table.status)
	}
	if _, err := records.Hold(context.Background(), zombie, domain.HoldCheckpoint); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("the dead worker's write = %v, want ErrFenceLost", err)
	}
	if err := records.Fail(context.Background(), zombie, domain.TurnFailed, "late", domain.TurnSettlement{}); !errors.Is(err, domain.ErrFenceLost) || table.status != domain.TurnRunning {
		t.Fatalf("the dead worker failed the resumed turn: %v, %s", err, table.status)
	}
	if !strings.Contains(turnRecordQueries[qRecordAcquire], sqlTurnQueued+", "+sqlTurnOwned) {
		t.Fatal("acquisition must take a queued turn and a running one redelivered under its current lease")
	}
}

// G1 through the runtime: a crash redelivery of a running turn resumes under fence +1 and
// completes; before, Start refused the running turn and the runtime failed it.
func TestTurnRuntimeResumesARunningTurnRedeliveredUnderItsCurrentLease(t *testing.T) {
	recorder := &runtimeRecorder{}
	runtime := newTestRuntime(t, recorder)
	table := newFenceTable()
	table.status, table.fence = domain.TurnRunning, 1
	runtime.Records = newFenceRecordStore(table)
	if _, err := runtime.HandleTurn(context.Background(), runtimeClaim()); err != nil {
		t.Fatalf("redelivery = %v", err)
	}
	if table.fence != 2 || len(recorder.failures) != 0 || !strings.Contains(strings.Join(recorder.calls, ","), "complete") {
		t.Fatalf("fence %d, failures %v, calls %v", table.fence, recorder.failures, recorder.calls)
	}
	for _, frame := range recorder.frames {
		if frame.Fence != 2 {
			t.Fatalf("frame published under fence %d, want 2", frame.Fence)
		}
	}
}

// §8.14: a delayed or forged claim neither acquires the turn nor moves its fence.
func TestTableTurnRecordStoreRefusesAStaleOrForgedClaim(t *testing.T) {
	table := newFenceTable()
	records := newFenceRecordStore(table)
	owner := table.acquire(t, records)
	for name, mutate := range map[string]func(*domain.TurnClaim){
		"delayed delivery": func(claim *domain.TurnClaim) { claim.LeaseToken = 98 },
		"another worker":   func(claim *domain.TurnClaim) { claim.WorkerID = "worker-9" },
		"another row":      func(claim *domain.TurnClaim) { claim.QueueID = 12 },
	} {
		claim := claimOf(fenceDispatch())
		mutate(&claim)
		if _, err := records.Acquire(context.Background(), claim); !errors.Is(err, domain.ErrFenceLost) {
			t.Fatalf("%s: %v, want ErrFenceLost", name, err)
		}
		if table.fence != owner.Fence {
			t.Fatalf("%s moved the fence to %d", name, table.fence)
		}
	}
	if _, err := records.Acquire(context.Background(), domain.TurnClaim{Dispatch: fenceDispatch()}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a claim without a lease = %v", err)
	}
	// A current delivery that finds the turn held is told so; a terminal one conflicts.
	if _, err := records.Hold(context.Background(), owner, domain.HoldCheckpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := records.Acquire(context.Background(), claimOf(fenceDispatch())); !errors.Is(err, domain.ErrTurnHeld) {
		t.Fatalf("held = %v, want ErrTurnHeld", err)
	}
	table.status = domain.TurnCompleted
	if _, err := records.Acquire(context.Background(), claimOf(fenceDispatch())); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("terminal = %v, want conflict", err)
	}
	if !strings.Contains(turnRecordQueries[qRecordAcquire], "lease_until > CURRENT_TIMESTAMP") {
		t.Fatal("an expired lease is not current, by the store clock")
	}
}

// G5: both claim paths order a conversation by its durable turn numbers and block behind every
// earlier live turn, suspended and held ones included; queue timestamps alone let a later turn
// run while an earlier one was parked.
func TestQueueClaimsBlockBehindEveryEarlierLiveTurn(t *testing.T) {
	queries, pending, claim, _ := TurnQueueWorkerQueries("runtime-1", 90, 25, 4)
	for name, query := range map[string]string{
		"fair claim": turnSchedulerQueries[qSchedClaim], "worker pending": queries[pending], "worker claim": queries[claim],
	} {
		if !strings.Contains(query, "earlier.turn_no < mine.turn_no") || !strings.Contains(query, "earlier.status_code IN ("+sqlTurnLive+")") {
			t.Errorf("%s does not block behind earlier live turns:\n%s", name, query)
		}
	}
	for _, code := range []string{sqlTurnSuspended, sqlTurnHeld} {
		if !strings.Contains(sqlTurnLive, code) {
			t.Errorf("live turns must include %s", code)
		}
	}
}

// The fenced statements carry the owner's guarded touch and nothing else locks.
func TestFencedStatementsCarryTheGuardedTouch(t *testing.T) {
	for name, query := range map[string]string{
		"journal append": loopJournalQueries[qLoopEntryInsert], "step claim": stepIdempotencyQueries[qStepClaim],
		"step commit": stepIdempotencyQueries[qStepCommit], "step abandon": stepIdempotencyQueries[qStepAbandon],
		"step replay": stepIdempotencyQueries[qStepReplay], "step reclaim": stepIdempotencyQueries[qStepReclaimExpired],
	} {
		if !strings.HasPrefix(strings.TrimSpace(query), "WITH owner AS (") || !strings.Contains(query, "fence = ?") {
			t.Errorf("%s has no guarded touch:\n%s", name, query)
		}
	}
	all := []string{turnOwnerGuard, turnRecordQueries[qRecordAcquire], turnRecordQueries[qRecordHold], turnControlQueries[qControlHold], turnControlQueries[qControlRelease]}
	for _, query := range all {
		if strings.Contains(query, "FOR UPDATE") || strings.Contains(query, "advisory") {
			t.Errorf("a fenced statement takes an explicit lock:\n%s", query)
		}
	}
	for name, query := range map[string]string{
		"owner fail": turnRecordQueries[qRecordFail], "suspend": turnRecordQueries[qRecordSuspend],
		"hold": turnRecordQueries[qRecordHold], "complete": durableSessionQueries[qSessionCompleteTurn],
	} {
		if !strings.Contains(query, "fence = ?") {
			t.Errorf("%s is not fenced:\n%s", name, query)
		}
	}
}
