package dataplane

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/keel/cache"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// controlTable is the one conversation_turn row every process sees, with the semantics of the
// turn control statements: first request wins, a parked turn is held or woken under a new fence.
type controlTable struct {
	keelport.DatabaseRepository
	keelport.QueryService
	mu         sync.Mutex
	status     domain.TurnStatus
	fence      int64
	reason     *string
	holdReason *string
	holdCode   domain.TurnHoldCode
	woken      []string
	readErr    error
	// openOutcomes counts the turn's in-flight, accepted, and unknown calls; unknown the ones a cancellation marked.
	openOutcomes int
	inFlight     int
	unknown      int
}

func newControlTable(status domain.TurnStatus, fence int64) *controlTable {
	return &controlTable{status: status, fence: fence}
}

func (table *controlTable) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return table, nil
}
func (*controlTable) GenID() int64                   { return 0 }
func (*controlTable) Commit(context.Context) error   { return nil }
func (*controlTable) Rollback(context.Context) error { return nil }

func (table *controlTable) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return table
}

func (table *controlTable) live() bool { return !table.status.Terminal() }

func (table *controlTable) unhold() {
	table.holdCode, table.holdReason = "", nil
}

func (table *controlTable) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	result := &keelmodel.QueryResult{}
	parked := table.status == domain.TurnQueued || table.status == domain.TurnSuspended
	running := table.status == domain.TurnRunning || table.status == domain.TurnStreaming
	switch name {
	case qControlCancelRequest:
		if table.live() {
			if table.reason == nil {
				reason := args[0].(string)
				table.reason = &reason
			}
			result.Rows = [][]any{{string(table.status)}}
		}
	case qControlWake:
		if table.status == domain.TurnSuspended || table.status == domain.TurnHeld {
			table.status, table.fence = domain.TurnQueued, table.fence+1
			table.unhold()
			result.Rows = [][]any{{int64(1)}}
		}
		table.woken = append(table.woken, name)
	case qQueueRequeue:
		table.woken = append(table.woken, name)
	case qControlRead:
		if table.readErr != nil {
			return nil, table.readErr
		}
		reason := ""
		if table.reason != nil {
			reason = *table.reason
		}
		result.Rows = [][]any{{reason, table.reason != nil, table.holdReason != nil, table.fence, string(table.status)}}
	case qControlHold:
		if table.reason != nil || !parked && !running {
			break
		}
		if parked {
			table.status, table.fence, table.holdCode, table.holdReason = domain.TurnHeld, table.fence+1, domain.TurnHoldCode(args[0].(string)), nil
		} else if table.holdReason == nil {
			reason := args[1].(string)
			table.holdReason = &reason
		}
		result.Rows = [][]any{{string(table.status), table.fence}}
	case qControlState:
		result.Rows = [][]any{{string(table.status), table.fence}}
	case qControlRelease:
		if table.status == domain.TurnHeld && table.fence == args[2] && table.openOutcomes == 0 {
			table.status, table.fence = domain.TurnQueued, table.fence+1
			table.unhold()
			result.Rows = [][]any{{table.fence}}
		}
	case qControlPreempt:
		if table.reason == nil && (parked || running) {
			table.status, table.fence, table.holdCode, table.holdReason = domain.TurnHeld, table.fence+1, domain.HoldPreempted, nil
			result.Rows = [][]any{{string(table.status), table.fence}}
		}
	case qControlUnknown:
		table.unknown, table.inFlight = table.unknown+table.inFlight, 0
		table.woken = append(table.woken, name)
	}
	return result, nil
}

func (table *controlTable) moveFence() {
	table.mu.Lock()
	defer table.mu.Unlock()
	table.fence++
}

func awaitCause(t *testing.T, ctx context.Context) error {
	t.Helper()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(5 * time.Second):
		t.Fatal("the turn context was never cancelled")
		return nil
	}
}

var controlOwner = domain.TurnOwner{TenantID: 7, RequestID: "request-1", Fence: 1}

func TestTableTurnControlStopsATurnRunningInAnotherProcess(t *testing.T) {
	table := newControlTable(domain.TurnRunning, 1)
	for name, shared := range map[string]cache.CacheService{"polling only": nil, "with cache wake-up": cache.NewMemoryCacheService()} {
		table.reason = nil
		api := &TableTurnControl{DB: table, Cache: shared}
		worker := &TableTurnControl{DB: table, Cache: shared, PollInterval: 10 * time.Millisecond}
		if shared != nil {
			worker.PollInterval = time.Hour
		}
		watch, err := worker.Watch(context.Background(), controlOwner)
		if err != nil {
			t.Fatalf("%s: Watch: %v", name, err)
		}
		if err = api.Cancel(context.Background(), 7, "request-1", "user stopped it"); err != nil {
			t.Fatalf("%s: Cancel: %v", name, err)
		}
		if cause := awaitCause(t, watch.Context()); !errors.Is(cause, domain.ErrTurnCanceled) {
			t.Fatalf("%s: cause = %v", name, cause)
		}
		watch.Release()
		worker.Close()
	}

	// The flag outlives the request: a turn picked up later is cancelled before it runs.
	late, err := (&TableTurnControl{DB: table}).Watch(context.Background(), controlOwner)
	if err != nil || !errors.Is(context.Cause(late.Context()), domain.ErrTurnCanceled) {
		t.Fatalf("a recorded cancellation must stop a later pickup: %v, %v", context.Cause(late.Context()), err)
	}
	late.Release()

	if slices.Contains(table.woken, qControlWake) || slices.Contains(table.woken, qQueueRequeue) {
		t.Fatalf("a running turn must not be requeued: %v", table.woken)
	}
	table.status = domain.TurnFailed
	if err = (&TableTurnControl{DB: table}).Cancel(context.Background(), 7, "request-1", "late"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound for a terminal turn, got %v", err)
	}
}

// Nothing runs a suspended or held turn, so cancelling one wakes it under a new fence and
// requeues it for a worker that ends it cancelled and releases its reservation.
func TestTableTurnControlWakesAParkedTurnItCancels(t *testing.T) {
	for _, status := range []domain.TurnStatus{domain.TurnSuspended, domain.TurnHeld} {
		table := newControlTable(status, 4)
		table.holdCode = domain.HoldCheckpoint
		if err := (&TableTurnControl{DB: table}).Cancel(context.Background(), 7, "request-1", "stop"); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if table.status != domain.TurnQueued || table.fence != 5 || table.holdCode != "" || !slices.Equal(table.woken, []string{qControlUnknown, qControlWake, qQueueRequeue}) {
			t.Fatalf("%s: status %s, fence %d, hold %q, woken %v", status, table.status, table.fence, table.holdCode, table.woken)
		}
	}
	if !strings.Contains(turnControlQueries[qControlCancelRequest], sqlTurnLive) {
		t.Fatal("every live turn, a held one included, must be cancellable")
	}
}

func TestTableTurnControlStopsATurnItCanNoLongerWatch(t *testing.T) {
	table := newControlTable(domain.TurnRunning, 1)
	watch, err := (&TableTurnControl{DB: table, PollInterval: 5 * time.Millisecond}).Watch(context.Background(), controlOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()
	table.mu.Lock()
	table.readErr = errors.New("connection refused")
	table.mu.Unlock()
	if cause := awaitCause(t, watch.Context()); cause == nil || errors.Is(cause, domain.ErrTurnCanceled) {
		t.Fatalf("an unreadable flag must stop the turn with the read error, got %v", cause)
	}
}

// A fence other than the owner's, as when another delivery acquired the turn or an operator
// held it, cancels the stale worker's context with ErrFenceLost, aborting its transports.
func TestTableTurnControlFencesOutAWorkerWhoseFenceMoved(t *testing.T) {
	table := newControlTable(domain.TurnRunning, 1)
	watch, err := (&TableTurnControl{DB: table, PollInterval: 5 * time.Millisecond}).Watch(context.Background(), controlOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()
	table.moveFence()
	if cause := awaitCause(t, watch.Context()); !errors.Is(cause, domain.ErrFenceLost) {
		t.Fatalf("cause = %v, want ErrFenceLost", cause)
	}
	if _, err := (&TableTurnControl{DB: table}).Watch(context.Background(), domain.TurnOwner{}); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("a watch without an owner = %v", err)
	}
}

// A hold request reaches the running worker as a flag, not a cancellation; the first reason stands.
func TestTableTurnControlAsksARunningTurnToHold(t *testing.T) {
	table := newControlTable(domain.TurnRunning, 1)
	control := &TableTurnControl{DB: table, PollInterval: 5 * time.Millisecond}
	watch, err := control.Watch(context.Background(), controlOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Release()
	if watch.HoldRequested() {
		t.Fatal("no hold was requested yet")
	}
	outcome, err := control.RequestHold(context.Background(), 7, "request-1", domain.HoldCheckpoint, "inspect the refund")
	if err != nil || outcome.Held() || outcome.Status != domain.TurnRunning || outcome.Fence != 1 {
		t.Fatalf("outcome = %+v, %v", outcome, err)
	}
	if _, err = control.RequestHold(context.Background(), 7, "request-1", domain.HoldCheckpoint, "later reason"); err != nil {
		t.Fatal(err)
	}
	if *table.holdReason != "inspect the refund" {
		t.Fatalf("the first request's reason must stand, got %q", *table.holdReason)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !watch.HoldRequested() {
		if time.Now().After(deadline) {
			t.Fatal("the watch never saw the hold request")
		}
		time.Sleep(time.Millisecond)
	}
	if err := context.Cause(watch.Context()); err != nil {
		t.Fatalf("a hold request must not cancel the turn: %v", err)
	}
	if _, err := control.RequestHold(context.Background(), 7, "request-1", "Z", "bad code"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown hold code = %v", err)
	}
}

func TestTableTurnControlHoldsAParkedTurnAtOnceAndReleasesItUnderANewFence(t *testing.T) {
	for _, status := range []domain.TurnStatus{domain.TurnQueued, domain.TurnSuspended} {
		table := newControlTable(status, 2)
		control := &TableTurnControl{DB: table}
		outcome, err := control.RequestHold(context.Background(), 7, "request-1", domain.HoldAgentPaused, "agent paused")
		if err != nil || !outcome.Held() || outcome.Fence != 3 || table.holdCode != domain.HoldAgentPaused {
			t.Fatalf("%s: outcome = %+v, code %q, %v", status, outcome, table.holdCode, err)
		}
		// Holding a held turn again is a no-op that reports it held.
		again, err := control.RequestHold(context.Background(), 7, "request-1", domain.HoldCheckpoint, "again")
		if err != nil || !again.Held() || again.Fence != 3 {
			t.Fatalf("%s: repeat = %+v, %v", status, again, err)
		}
		if _, err := control.Release(context.Background(), 7, "request-1", 2); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("%s: a stale fence must not release, got %v", status, err)
		}
		fence, err := control.Release(context.Background(), 7, "request-1", 3)
		if err != nil || fence != 4 || table.status != domain.TurnQueued || table.holdCode != "" || !slices.Equal(table.woken, []string{qQueueRequeue}) {
			t.Fatalf("%s: release fence %d, status %s, woken %v, %v", status, fence, table.status, table.woken, err)
		}
	}
	cancelled := newControlTable(domain.TurnQueued, 1)
	reason := "stop"
	cancelled.reason = &reason
	if _, err := (&TableTurnControl{DB: cancelled}).RequestHold(context.Background(), 7, "request-1", domain.HoldCheckpoint, "hold"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("a cancelled turn cannot be held, got %v", err)
	}
}
