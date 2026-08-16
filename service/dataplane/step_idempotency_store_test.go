package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

var (
	stepNow   = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	stepLease = time.Minute
	testStep  = domain.ExecutionStep{ExecutionStepID: 91, StepID: "plan", Kind: "model"}
)

func newStepStore(query *persistenceQueryFake, storage *fake.ObjectStorage) *StepIdempotencyStore {
	return &StepIdempotencyStore{DB: persistenceDBFake{query: query}, Objects: newObjectStateStore(storage),
		ClaimLease: stepLease, Now: func() time.Time { return stepNow }}
}

func TestStepIdempotencyStoreBeginClaimsUnknownStep(t *testing.T) {
	query := &persistenceQueryFake{rows: map[string][][]any{qStepClaim: {{"claimed"}}}}
	result, replayed, err := newStepStore(query, &fake.ObjectStorage{}).Begin(context.Background(), 7, "req", testStep)
	if err != nil || replayed || !reflect.DeepEqual(result, domain.StepResult{}) {
		t.Fatalf("begin = %+v, %v, %v", result, replayed, err)
	}
	if args := query.lastArgs(qStepFind); !reflect.DeepEqual(args, []any{int64(7), "req", int64(91)}) {
		t.Fatalf("find args = %v", args)
	}
	if args := query.lastArgs(qStepClaim); !reflect.DeepEqual(args, []any{int64(7), "req", int64(91), stepNow}) {
		t.Fatalf("claim args = %v", args)
	}
	// Lost the insert race.
	raced := &persistenceQueryFake{}
	if _, _, err := newStepStore(raced, &fake.ObjectStorage{}).Begin(context.Background(), 7, "req", testStep); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("raced = %v", err)
	}
}

func TestStepIdempotencyStoreBeginReplaysCommittedResult(t *testing.T) {
	storage := &fake.ObjectStorage{}
	codec := newObjectStateStore(storage)
	want := domain.StepResult{State: []byte(`{"a":1}`), NextStepID: "answer", Fingerprint: testFingerprint,
		Usage: domain.Usage{InputTokens: 3, CostMinorUnits: 2, Currency: "USD"}}
	payload, _ := json.Marshal(want)
	ref, err := codec.Dehydrate(context.Background(), stepResultName(7, "req", 91), payload)
	if err != nil {
		t.Fatal(err)
	}
	query := &persistenceQueryFake{rows: map[string][][]any{qStepFind: {{"committed", ref.URI, ref.Digest, stepNow.Add(-time.Hour)}}}}
	result, replayed, err := newStepStore(query, storage).Begin(context.Background(), 7, "req", testStep)
	if err != nil || !replayed || !reflect.DeepEqual(result, want) {
		t.Fatalf("replay = %+v, %v, %v", result, replayed, err)
	}
	if len(query.calls) != 1 {
		t.Fatalf("replay must not transition: %v", query.calls)
	}
	// Digest drift fails closed instead of replaying corrupt state.
	storage.Overwrite("sessions", objectKeyOf(ref), []byte(`{"a":2}`))
	if _, _, err := newStepStore(query, storage).Begin(context.Background(), 7, "req", testStep); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered = %v", err)
	}
}

func TestStepIdempotencyStoreBeginLeaseAndReplayTransitions(t *testing.T) {
	fresh := &persistenceQueryFake{rows: map[string][][]any{qStepFind: {{"claimed", nil, nil, stepNow.Add(-stepLease / 2)}}}}
	if _, _, err := newStepStore(fresh, &fake.ObjectStorage{}).Begin(context.Background(), 7, "req", testStep); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("live claim = %v", err)
	}
	if len(fresh.calls) != 1 {
		t.Fatalf("live claim must not transition: %v", fresh.calls)
	}

	expired := &persistenceQueryFake{rows: map[string][][]any{
		qStepFind:           {{"claimed", nil, nil, stepNow.Add(-stepLease)}},
		qStepReclaimExpired: {{"claimed"}},
	}}
	if _, replayed, err := newStepStore(expired, &fake.ObjectStorage{}).Begin(context.Background(), 7, "req", testStep); err != nil || replayed {
		t.Fatalf("expired = %v, %v", replayed, err)
	}
	if args := expired.lastArgs(qStepReclaimExpired); !reflect.DeepEqual(args, []any{stepNow, int64(7), "req", int64(91), stepNow.Add(-stepLease)}) {
		t.Fatalf("reclaim args = %v", args)
	}
	lostReclaim := &persistenceQueryFake{rows: map[string][][]any{qStepFind: {{"claimed", nil, nil, stepNow.Add(-stepLease)}}}}
	if _, _, err := newStepStore(lostReclaim, &fake.ObjectStorage{}).Begin(context.Background(), 7, "req", testStep); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("lost reclaim = %v", err)
	}

	abandoned := &persistenceQueryFake{rows: map[string][][]any{
		qStepFind:   {{"abandoned", nil, nil, stepNow.Add(-time.Second)}},
		qStepReplay: {{"claimed"}},
	}}
	if _, replayed, err := newStepStore(abandoned, &fake.ObjectStorage{}).Begin(context.Background(), 7, "req", testStep); err != nil || replayed {
		t.Fatalf("abandoned = %v, %v", replayed, err)
	}
	if args := abandoned.lastArgs(qStepReplay); !reflect.DeepEqual(args, []any{stepNow, int64(7), "req", int64(91)}) {
		t.Fatalf("replay args = %v", args)
	}
}

func TestStepIdempotencyStoreCommitBindsResultAndTolerantOfDuplicates(t *testing.T) {
	storage := &fake.ObjectStorage{}
	query := &persistenceQueryFake{rows: map[string][][]any{qStepCommit: {{"committed"}}}}
	result := domain.StepResult{State: []byte("s"), NextStepID: "n", Fingerprint: testFingerprint}
	if err := newStepStore(query, storage).Commit(context.Background(), 7, "req", testStep, result); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(result)
	digest := DigestBytes(payload)
	uri := "scout://sessions/scout/step/7/req/91/" + digest
	if args := query.lastArgs(qStepCommit); !reflect.DeepEqual(args, []any{uri, digest, stepNow, int64(7), "req", int64(91)}) {
		t.Fatalf("commit args = %v", args)
	}
	// Duplicate delivery of the same result is a no-op and keeps the object.
	duplicate := &persistenceQueryFake{rows: map[string][][]any{qStepFind: {{"committed", uri, digest, stepNow}}}}
	if err := newStepStore(duplicate, storage).Commit(context.Background(), 7, "req", testStep, result); err != nil {
		t.Fatalf("duplicate = %v", err)
	}
	if len(storage.Deletes) != 0 {
		t.Fatalf("duplicate deleted the shared object: %v", storage.Deletes)
	}
	// A lost claim (another worker committed different content) is a conflict; our orphan goes.
	lost := &persistenceQueryFake{rows: map[string][][]any{qStepFind: {{"committed", "scout://sessions/other", testFingerprint, stepNow}}}}
	other := &fake.ObjectStorage{}
	if err := newStepStore(lost, other).Commit(context.Background(), 7, "req", testStep, result); !errors.Is(err, domain.ErrConflict) || len(other.Deletes) != 1 {
		t.Fatalf("lost = %v, deletes = %v", err, other.Deletes)
	}
}

func TestStepIdempotencyStoreAbandon(t *testing.T) {
	query := &persistenceQueryFake{rows: map[string][][]any{qStepAbandon: {{"abandoned"}}}}
	if err := newStepStore(query, &fake.ObjectStorage{}).Abandon(context.Background(), 7, "req", testStep); err != nil {
		t.Fatal(err)
	}
	if args := query.lastArgs(qStepAbandon); !reflect.DeepEqual(args, []any{stepNow, int64(7), "req", int64(91)}) {
		t.Fatalf("abandon args = %v", args)
	}
	twice := &persistenceQueryFake{rows: map[string][][]any{qStepFind: {{"abandoned", nil, nil, stepNow}}}}
	if err := newStepStore(twice, &fake.ObjectStorage{}).Abandon(context.Background(), 7, "req", testStep); err != nil {
		t.Fatalf("second abandon = %v", err)
	}
	committed := &persistenceQueryFake{rows: map[string][][]any{qStepFind: {{"committed", "u", testFingerprint, stepNow}}}}
	if err := newStepStore(committed, &fake.ObjectStorage{}).Abandon(context.Background(), 7, "req", testStep); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("committed abandon = %v", err)
	}
	if err := newStepStore(&persistenceQueryFake{}, &fake.ObjectStorage{}).Abandon(context.Background(), 7, "req", testStep); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("unknown abandon must be not found")
	}
}

func TestStepIdempotencyStoreValidation(t *testing.T) {
	store := newStepStore(&persistenceQueryFake{}, &fake.ObjectStorage{})
	if _, _, err := store.Begin(context.Background(), 7, "req", domain.ExecutionStep{StepID: "plan"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing execution step id = %v", err)
	}
	unleased := &StepIdempotencyStore{DB: persistenceDBFake{query: &persistenceQueryFake{}}, Objects: newObjectStateStore(&fake.ObjectStorage{})}
	if _, _, err := unleased.Begin(context.Background(), 7, "req", testStep); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("missing lease = %v", err)
	}
}

// stepTableFake emulates the step_idempotency row transitions so a whole
// claim → crash → replay scenario runs against real store logic.
type stepTableFake struct {
	mu   sync.Mutex
	rows map[[3]any]*stepRow
}

type stepRow struct {
	status    string
	uri       string
	digest    string
	updatedAt time.Time
}

func (table *stepTableFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.rows == nil {
		table.rows = make(map[[3]any]*stepRow)
	}
	key := func(i int) [3]any { return [3]any{args[i], args[i+1], args[i+2]} }
	switch name {
	case qStepFind:
		if row := table.rows[key(0)]; row != nil {
			return &keelmodel.QueryResult{Rows: [][]any{{row.status, row.uri, row.digest, row.updatedAt}}}, nil
		}
	case qStepClaim:
		if table.rows[key(0)] == nil {
			table.rows[key(0)] = &stepRow{status: "claimed", updatedAt: args[3].(time.Time)}
			return &keelmodel.QueryResult{Rows: [][]any{{"claimed"}}}, nil
		}
	case qStepReclaimExpired:
		if row := table.rows[key(1)]; row != nil && row.status == "claimed" && !row.updatedAt.After(args[4].(time.Time)) {
			row.updatedAt = args[0].(time.Time)
			return &keelmodel.QueryResult{Rows: [][]any{{"claimed"}}}, nil
		}
	case qStepReplay:
		if row := table.rows[key(1)]; row != nil && row.status == "abandoned" {
			row.status, row.updatedAt = "claimed", args[0].(time.Time)
			return &keelmodel.QueryResult{Rows: [][]any{{"claimed"}}}, nil
		}
	case qStepCommit:
		if row := table.rows[key(3)]; row != nil && row.status == "claimed" {
			row.status, row.uri, row.digest, row.updatedAt = "committed", args[0].(string), args[1].(string), args[2].(time.Time)
			return &keelmodel.QueryResult{Rows: [][]any{{"committed"}}}, nil
		}
	case qStepAbandon:
		if row := table.rows[key(1)]; row != nil && row.status == "claimed" {
			row.status, row.updatedAt = "abandoned", args[0].(time.Time)
			return &keelmodel.QueryResult{Rows: [][]any{{"abandoned"}}}, nil
		}
	}
	return &keelmodel.QueryResult{}, nil
}

func (*stepTableFake) GenID() int64                   { return 0 }
func (*stepTableFake) Commit(context.Context) error   { return nil }
func (*stepTableFake) Rollback(context.Context) error { return nil }

type stepTableDB struct {
	keelport.DatabaseRepository
	table *stepTableFake
}

func (db stepTableDB) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.table
}

func (db stepTableDB) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.table, nil
}

func TestStepIdempotencyStoreCrashMidStepReplaysResultNotSideEffect(t *testing.T) {
	table := &stepTableFake{}
	storage := &fake.ObjectStorage{}
	clock := stepNow
	store := &StepIdempotencyStore{DB: stepTableDB{table: table}, Objects: newObjectStateStore(storage),
		ClaimLease: stepLease, Now: func() time.Time { return clock }}
	ctx := context.Background()
	sideEffects := 0
	execute := func() (domain.StepResult, error) {
		if _, replayed, err := store.Begin(ctx, 7, "req", testStep); err != nil {
			return domain.StepResult{}, err
		} else if replayed {
			t.Fatal("first execution must not replay")
		}
		sideEffects++
		result := domain.StepResult{State: []byte(`{"tool":"called"}`), NextStepID: "answer", Fingerprint: testFingerprint}
		return result, store.Commit(ctx, 7, "req", testStep, result)
	}
	// Worker A commits, then crashes before acking. Worker B redelivers.
	committed, err := execute()
	if err != nil {
		t.Fatal(err)
	}
	replayed, ok, err := store.Begin(ctx, 7, "req", testStep)
	if err != nil || !ok || !reflect.DeepEqual(replayed, committed) || sideEffects != 1 {
		t.Fatalf("redelivery = %+v, %v, %v, side effects = %d", replayed, ok, err, sideEffects)
	}
	// Duplicate commit of the same result is a no-op.
	if err := store.Commit(ctx, 7, "req", testStep, committed); err != nil {
		t.Fatalf("duplicate commit = %v", err)
	}
	if err := store.Abandon(ctx, 7, "req", testStep); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("abandon after commit = %v", err)
	}

	// A different step: worker C claims and dies mid-step; the lease blocks D until it expires.
	other := domain.ExecutionStep{ExecutionStepID: 92, StepID: "answer"}
	if _, _, err := store.Begin(ctx, 7, "req", other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Begin(ctx, 7, "req", other); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("live lease = %v", err)
	}
	clock = clock.Add(stepLease)
	if _, replayed, err := store.Begin(ctx, 7, "req", other); err != nil || replayed {
		t.Fatalf("expired reclaim = %v, %v", replayed, err)
	}
	// Explicit abandon then replay by another worker.
	if err := store.Abandon(ctx, 7, "req", other); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := store.Begin(ctx, 7, "req", other); err != nil || replayed {
		t.Fatalf("abandoned replay = %v, %v", replayed, err)
	}
}
