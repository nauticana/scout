package confirmation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestTableStoreBatchTakesItsLimits(t *testing.T) {
	for name, test := range map[string]struct {
		store       *TableStore
		limit, want int
	}{
		"zero default":     {&TableStore{}, 0, DefaultBatch},
		"zero cap":         {&TableStore{}, 5000, DefaultMaxBatch},
		"named size":       {&TableStore{}, 7, 7},
		"own default":      {&TableStore{Batch: 10, MaxBatch: 20}, 0, 10},
		"own cap":          {&TableStore{Batch: 10, MaxBatch: 20}, 50, 20},
		"within own range": {&TableStore{Batch: 10, MaxBatch: 20}, 15, 15},
	} {
		if got := test.store.batch(test.limit); got != test.want {
			t.Errorf("%s: batch(%d) = %d, want %d", name, test.limit, got, test.want)
		}
	}
}

func TestTableStoreRefusesInvalidLimits(t *testing.T) {
	for name, store := range map[string]*TableStore{
		"negative document bytes":   {MaxDocumentBytes: -1},
		"negative batch":            {Batch: -1},
		"negative max batch":        {MaxBatch: -1},
		"batch above its cap":       {Batch: 30, MaxBatch: 20},
		"batch above the default":   {Batch: DefaultMaxBatch + 1},
		"default batch above a cap": {MaxBatch: DefaultBatch - 1},
	} {
		store.DB = newConfirmationDB()
		if _, _, err := store.Prepare(context.Background(), draft(domain.MCPConfirmationInbox)); err == nil || errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: want a composition error, got %v", name, err)
		}
		if _, err := store.List(context.Background(), 7, time.Time{}, 0); err == nil {
			t.Errorf("%s: List must refuse too", name)
		}
	}
}

func TestTableStoreTakesItsDocumentLimit(t *testing.T) {
	store := &TableStore{DB: newConfirmationDB(), MaxDocumentBytes: 24}
	if _, _, err := store.Prepare(context.Background(), draft(domain.MCPConfirmationInbox)); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a preview over 24 bytes: want ErrValidation, got %v", err)
	}
	store.MaxDocumentBytes = 64
	if _, _, err := store.Prepare(context.Background(), draft(domain.MCPConfirmationInbox)); err != nil {
		t.Fatalf("within the limit: %v", err)
	}
	if _, err := ValidateDraft(draft(domain.MCPConfirmationInbox), -1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative limit: %v", err)
	}
}

// leaseRunner records the claim's lease while it runs.
type leaseRunner struct {
	db    *confirmationDB
	until time.Time
}

func (r *leaseRunner) RunConfirmed(_ context.Context, stored domain.MCPConfirmation) (json.RawMessage, error) {
	r.until = r.db.row(stored.ID).leaseUntil
	return json.RawMessage(`{"ok":true}`), nil
}

func TestExecutorTakesItsLeaseAndResultLimit(t *testing.T) {
	ctx := context.Background()
	run := func(executor *Executor, f *fixture) (domain.MCPConfirmation, error) {
		prepared, _, err := f.store.Prepare(ctx, draft(domain.MCPConfirmationInbox))
		if err != nil {
			t.Fatal(err)
		}
		key := domain.MCPConfirmationKey{TenantID: 7, ID: prepared.ID}
		if _, err := executor.Decide(ctx, key, checker, true, ""); err != nil {
			t.Fatal(err)
		}
		return executor.Execute(ctx, key)
	}
	for lease, want := range map[time.Duration]time.Duration{0: DefaultLease, 10 * time.Second: 10 * time.Second} {
		f := newFixture()
		runner := &leaseRunner{db: f.db}
		f.executor.Runner, f.executor.Lease = runner, lease
		if stored, err := run(f.executor, f); err != nil || stored.Status != domain.MCPConfirmationExecuted {
			t.Fatalf("lease %v: %+v, %v", lease, stored, err)
		}
		if runner.until != f.db.now.Add(want) {
			t.Fatalf("lease %v: claimed until %v, want %v", lease, runner.until, f.db.now.Add(want))
		}
	}
	f := newFixture()
	f.executor.MaxDocumentBytes = len(`{"published":true}`) - 1
	if stored, err := run(f.executor, f); err != nil || stored.Status != domain.MCPConfirmationUnknown {
		t.Fatalf("a result over the executor's limit is unknown: %+v, %v", stored, err)
	}
	for name, executor := range map[string]func(*Executor){
		"negative lease":          func(e *Executor) { e.Lease = -time.Second },
		"negative document bytes": func(e *Executor) { e.MaxDocumentBytes = -1 },
	} {
		f := newFixture()
		executor(f.executor)
		if _, err := run(f.executor, f); err == nil || !strings.Contains(err.Error(), "cannot be negative") || f.runner.calls.Load() != 0 {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
