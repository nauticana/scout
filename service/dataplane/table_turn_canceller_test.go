package dataplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nauticana/keel/cache"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// cancelTable is the one conversation_turn row both processes see.
type cancelTable struct {
	keelport.DatabaseRepository
	keelport.QueryService
	mu      sync.Mutex
	live    bool
	status  string
	woken   []string
	reason  *string
	readErr error
}

func (table *cancelTable) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return table, nil
}
func (*cancelTable) GenID() int64                   { return 0 }
func (*cancelTable) Commit(context.Context) error   { return nil }
func (*cancelTable) Rollback(context.Context) error { return nil }

func (table *cancelTable) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return table
}

func (table *cancelTable) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	result := &keelmodel.QueryResult{}
	switch {
	case name == qCancelRequest && table.live:
		if table.reason == nil {
			reason := args[0].(string)
			table.reason = &reason
		}
		result.Rows = [][]any{{table.status}}
	case name == qCancelWake || name == qQueueRequeue:
		table.woken = append(table.woken, name)
	case name == qCancelRead && table.readErr != nil:
		return nil, table.readErr
	case name == qCancelRead && table.reason != nil:
		result.Rows = [][]any{{*table.reason}}
	}
	return result, nil
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

func TestTableTurnCancellerStopsATurnRunningInAnotherProcess(t *testing.T) {
	table := &cancelTable{live: true, status: "running"}
	for name, shared := range map[string]cache.CacheService{"polling only": nil, "with cache wake-up": cache.NewMemoryCacheService()} {
		table.reason = nil
		api := &TableTurnCanceller{DB: table, Cache: shared}
		worker := &TableTurnCanceller{DB: table, Cache: shared, PollInterval: 10 * time.Millisecond}
		if shared != nil {
			worker.PollInterval = time.Hour
		}
		turnCtx, release, err := worker.Watch(context.Background(), 7, "request-1")
		if err != nil {
			t.Fatalf("%s: Watch: %v", name, err)
		}
		if err = api.Cancel(context.Background(), 7, "request-1", "user stopped it"); err != nil {
			t.Fatalf("%s: Cancel: %v", name, err)
		}
		if cause := awaitCause(t, turnCtx); !errors.Is(cause, domain.ErrTurnCanceled) {
			t.Fatalf("%s: cause = %v", name, cause)
		}
		release()
		worker.Close()
	}

	// The flag outlives the request: a turn picked up later is cancelled before it runs.
	late, release, err := (&TableTurnCanceller{DB: table}).Watch(context.Background(), 7, "request-1")
	if err != nil || !errors.Is(context.Cause(late), domain.ErrTurnCanceled) {
		t.Fatalf("a recorded cancellation must stop a later pickup: %v, %v", context.Cause(late), err)
	}
	release()

	// Nothing runs a suspended turn, so cancelling one sends it back to a worker to be settled.
	if len(table.woken) != 0 {
		t.Fatalf("a running turn must not be requeued: %v", table.woken)
	}
	table.status = "suspended"
	if err = (&TableTurnCanceller{DB: table}).Cancel(context.Background(), 7, "request-1", "again"); err != nil || len(table.woken) != 2 {
		t.Fatalf("a suspended turn must be resumed and requeued: %v, %v", table.woken, err)
	}

	table.live = false
	if err = (&TableTurnCanceller{DB: table}).Cancel(context.Background(), 7, "request-1", "late"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound for a terminal turn, got %v", err)
	}
}

func TestTableTurnCancellerStopsATurnItCanNoLongerWatch(t *testing.T) {
	table := &cancelTable{live: true}
	turnCtx, release, err := (&TableTurnCanceller{DB: table, PollInterval: 5 * time.Millisecond}).Watch(context.Background(), 7, "request-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	table.mu.Lock()
	table.readErr = errors.New("connection refused")
	table.mu.Unlock()
	if cause := awaitCause(t, turnCtx); cause == nil || errors.Is(cause, domain.ErrTurnCanceled) {
		t.Fatalf("an unreadable flag must stop the turn with the read error, got %v", cause)
	}
}
