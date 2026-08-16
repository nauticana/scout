package dataplane

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

// persistenceQueryFake answers named queries from canned rows, records every
// argument list in call order, and can fail one query by name.
type persistenceQueryFake struct {
	rows      map[string][][]any
	errs      map[string]error
	calls     []string
	args      map[string][][]any
	commits   int
	rollbacks int
	commitErr error
}

func (query *persistenceQueryFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if query.args == nil {
		query.args = make(map[string][][]any)
	}
	query.calls = append(query.calls, name)
	query.args[name] = append(query.args[name], append([]any(nil), args...))
	if err := query.errs[name]; err != nil {
		return nil, err
	}
	return &keelmodel.QueryResult{Rows: query.rows[name]}, nil
}

func (*persistenceQueryFake) GenID() int64 { return 0 }
func (query *persistenceQueryFake) Commit(context.Context) error {
	query.commits++
	return query.commitErr
}
func (query *persistenceQueryFake) Rollback(context.Context) error {
	query.rollbacks++
	return nil
}

func (query *persistenceQueryFake) lastArgs(name string) []any {
	lists := query.args[name]
	if len(lists) == 0 {
		return nil
	}
	return lists[len(lists)-1]
}

type persistenceDBFake struct {
	keelport.DatabaseRepository
	query *persistenceQueryFake
}

func (db persistenceDBFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db.query
}

func (db persistenceDBFake) BeginTx(context.Context, map[string]string) (keelport.TxQueryService, error) {
	return db.query, nil
}

const testFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newDurableSessionStore(query *persistenceQueryFake, storage *fake.ObjectStorage) *DurableSessionStore {
	return &DurableSessionStore{DB: persistenceDBFake{query: query}, Objects: newObjectStateStore(storage)}
}

func validCheckpoint(state string) domain.StepCheckpoint {
	return domain.StepCheckpoint{
		ConversationID: "conv", TurnNo: 4, StepNo: 2, ExecutionStepID: 91, StepID: "plan",
		IdempotencyKey: "req-1:91", Fingerprint: testFingerprint, State: []byte(state),
		Usage: domain.Usage{InputTokens: 10, OutputTokens: 5, ToolCalls: 1, CostMinorUnits: 3, Currency: "USD"},
	}
}

func objectKeyOf(ref domain.ObjectRef) string {
	return strings.TrimPrefix(ref.URI, "scout://sessions/")
}

func TestDurableSessionStoreLoadHydratesAndVerifies(t *testing.T) {
	storage := &fake.ObjectStorage{}
	codec := newObjectStateStore(storage)
	ref, err := codec.Dehydrate(context.Background(), checkpointStateName(7, "conv", 4, 2), []byte(`{"memory":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	query := &persistenceQueryFake{rows: map[string][][]any{
		qSessionLoad: {{"v3", int64(4), int64(2), ref.URI, ref.Digest, int64(9), "plan"}},
	}}
	store := newDurableSessionStore(query, storage)
	snapshot, err := store.Load(context.Background(), 7, "conv")
	if err != nil {
		t.Fatal(err)
	}
	want := domain.SessionSnapshot{ConversationID: "conv", AgentVersion: "v3", LatestTurnNo: 4, LatestStepNo: 2,
		LastCompletedStepID: "plan", State: []byte(`{"memory":"x"}`), StateRef: ref, Revision: 9}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, want)
	}
	if args := query.lastArgs(qSessionLoad); !reflect.DeepEqual(args, []any{int64(7), "conv"}) {
		t.Fatalf("load args = %v", args)
	}
	// Tampered state fails closed.
	storage.Overwrite("sessions", objectKeyOf(ref), []byte(`{"memory":"y"}`))
	if _, err := store.Load(context.Background(), 7, "conv"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered load = %v", err)
	}
}

func TestDurableSessionStoreLoadFreshAndMissingConversations(t *testing.T) {
	fresh := newDurableSessionStore(&persistenceQueryFake{rows: map[string][][]any{
		qSessionLoad: {{"v1", nil, nil, nil, nil, nil, nil}},
	}}, &fake.ObjectStorage{})
	snapshot, err := fresh.Load(context.Background(), 7, "conv")
	if err != nil || snapshot.Revision != 0 || snapshot.AgentVersion != "v1" || snapshot.State != nil || snapshot.ConversationID != "conv" {
		t.Fatalf("fresh = %+v, %v", snapshot, err)
	}
	missing := newDurableSessionStore(&persistenceQueryFake{}, &fake.ObjectStorage{})
	if _, err := missing.Load(context.Background(), 7, "conv"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := missing.Load(context.Background(), 0, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid = %v", err)
	}
}

func TestDurableSessionStoreCheckpointCreatesThenAdvances(t *testing.T) {
	storage := &fake.ObjectStorage{}
	query := &persistenceQueryFake{rows: map[string][][]any{
		qSessionCreateSnapshot:  {{int64(1)}},
		qSessionAdvanceSnapshot: {{int64(6)}},
	}}
	store := newDurableSessionStore(query, storage)
	checkpoint := validCheckpoint(`{"s":1}`)
	if err := store.Checkpoint(context.Background(), 7, 0, checkpoint); err != nil {
		t.Fatal(err)
	}
	digest := DigestBytes(checkpoint.State)
	uri := "scout://sessions/scout/checkpoint/7/conv/4/2/" + digest
	wantInsert := []any{int64(7), "conv", int64(4), 2, int64(91), "req-1:91", uri, digest, testFingerprint,
		int64(10), int64(5), 1, int64(3), "USD"}
	if args := query.lastArgs(qSessionInsertCheckpoint); !reflect.DeepEqual(args, wantInsert) {
		t.Fatalf("insert args = %v\nwant %v", args, wantInsert)
	}
	if args := query.lastArgs(qSessionCreateSnapshot); !reflect.DeepEqual(args, []any{int64(7), "conv", int64(4), 2, uri, digest}) {
		t.Fatalf("create args = %v", args)
	}
	if !reflect.DeepEqual(query.calls, []string{qSessionInsertCheckpoint, qSessionCreateSnapshot}) || query.commits != 1 || query.rollbacks != 0 {
		t.Fatalf("calls = %v, commits = %d, rollbacks = %d", query.calls, query.commits, query.rollbacks)
	}
	if _, ok := storage.Payload("sessions", "scout/checkpoint/7/conv/4/2/"+digest); !ok {
		t.Fatal("state object missing")
	}

	if err := store.Checkpoint(context.Background(), 7, 5, checkpoint); err != nil {
		t.Fatal(err)
	}
	if args := query.lastArgs(qSessionAdvanceSnapshot); !reflect.DeepEqual(args, []any{int64(4), 2, uri, digest, int64(7), "conv", int64(5)}) {
		t.Fatalf("advance args = %v", args)
	}
	if query.commits != 2 {
		t.Fatalf("commits = %d", query.commits)
	}
}

func TestDurableSessionStoreCheckpointRevisionConflictKeepsSharedObject(t *testing.T) {
	storage := &fake.ObjectStorage{}
	checkpoint := validCheckpoint(`{"s":1}`)
	digest := DigestBytes(checkpoint.State)
	// The winning writer committed identical content: the object stays.
	query := &persistenceQueryFake{rows: map[string][][]any{qSessionCheckpointDigest: {{digest}}}}
	err := newDurableSessionStore(query, storage).Checkpoint(context.Background(), 7, 5, checkpoint)
	if !errors.Is(err, domain.ErrRevisionConflict) || query.rollbacks != 1 || query.commits != 0 {
		t.Fatalf("conflict = %v, rollbacks = %d, commits = %d", err, query.rollbacks, query.commits)
	}
	if len(storage.Deletes) != 0 || len(storage.Objects) != 1 {
		t.Fatalf("shared object was deleted: %v", storage.Deletes)
	}
	// The winning writer committed different content: our upload is an orphan and goes.
	other := &fake.ObjectStorage{}
	query = &persistenceQueryFake{rows: map[string][][]any{qSessionCheckpointDigest: {{strings.Repeat("f", 64)}}}}
	err = newDurableSessionStore(query, other).Checkpoint(context.Background(), 7, 5, checkpoint)
	if !errors.Is(err, domain.ErrRevisionConflict) || len(other.Deletes) != 1 || len(other.Objects) != 0 {
		t.Fatalf("conflict = %v, deletes = %v", err, other.Deletes)
	}
}

func TestDurableSessionStoreCheckpointFailureDeletesUploadAndJoinsErrors(t *testing.T) {
	dbErr := errors.New("db down")
	deleteErr := errors.New("bucket unreachable")
	storage := &fake.ObjectStorage{DeleteFunc: func(context.Context, string, string) error { return deleteErr }}
	query := &persistenceQueryFake{errs: map[string]error{qSessionInsertCheckpoint: dbErr}}
	err := newDurableSessionStore(query, storage).Checkpoint(context.Background(), 7, 5, validCheckpoint("x"))
	if !errors.Is(err, dbErr) || !errors.Is(err, deleteErr) || query.rollbacks != 1 {
		t.Fatalf("error = %v, rollbacks = %d", err, query.rollbacks)
	}
	// Commit failure without a reference check row: object removed best effort.
	clean := &fake.ObjectStorage{}
	query = &persistenceQueryFake{rows: map[string][][]any{qSessionAdvanceSnapshot: {{int64(6)}}}, commitErr: dbErr}
	err = newDurableSessionStore(query, clean).Checkpoint(context.Background(), 7, 5, validCheckpoint("x"))
	if !errors.Is(err, dbErr) || len(clean.Deletes) != 1 || len(clean.Objects) != 0 {
		t.Fatalf("commit failure = %v, deletes = %v", err, clean.Deletes)
	}
}

func TestDurableSessionStoreCheckpointNeverInventsFingerprintOrCurrency(t *testing.T) {
	store := newDurableSessionStore(&persistenceQueryFake{}, &fake.ObjectStorage{})
	cases := map[string]func(*domain.StepCheckpoint){
		"fingerprint": func(c *domain.StepCheckpoint) { c.Fingerprint = "" },
		"currency":    func(c *domain.StepCheckpoint) { c.Usage.Currency = "" },
		"idempotency": func(c *domain.StepCheckpoint) { c.IdempotencyKey = "" },
		"step id":     func(c *domain.StepCheckpoint) { c.ExecutionStepID = 0 },
		"usage":       func(c *domain.StepCheckpoint) { c.Usage.CostMinorUnits = -1 },
		"state":       func(c *domain.StepCheckpoint) { c.State = nil },
	}
	for name, mutate := range cases {
		checkpoint := validCheckpoint("x")
		mutate(&checkpoint)
		if err := store.Checkpoint(context.Background(), 7, 1, checkpoint); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := store.Checkpoint(context.Background(), 7, -1, validCheckpoint("x")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative revision: %v", err)
	}
}

func TestDurableSessionStoreCheckpointAcceptsDehydratedReference(t *testing.T) {
	storage := &fake.ObjectStorage{}
	query := &persistenceQueryFake{rows: map[string][][]any{qSessionAdvanceSnapshot: {{int64(2)}}}}
	checkpoint := validCheckpoint("")
	checkpoint.State = nil
	checkpoint.StateRef = domain.ObjectRef{URI: "scout://sessions/elsewhere", Digest: testFingerprint}
	if err := newDurableSessionStore(query, storage).Checkpoint(context.Background(), 7, 1, checkpoint); err != nil {
		t.Fatal(err)
	}
	if len(storage.Uploads) != 0 || query.lastArgs(qSessionInsertCheckpoint)[6] != "scout://sessions/elsewhere" {
		t.Fatalf("uploads = %v, args = %v", storage.Uploads, query.lastArgs(qSessionInsertCheckpoint))
	}
}

func TestDurableSessionStoreCompleteGuardsRevisionAndStatus(t *testing.T) {
	storage := &fake.ObjectStorage{}
	query := &persistenceQueryFake{rows: map[string][][]any{
		qSessionActiveTurn:   {{int64(4), int64(6)}},
		qSessionCompleteTurn: {{int64(4)}},
	}}
	store := newDurableSessionStore(query, storage)
	response := []byte(`{"text":"done"}`)
	if err := store.Complete(context.Background(), 7, "conv", 6, domain.TurnResult{Response: response}); err != nil {
		t.Fatal(err)
	}
	digest := DigestBytes(response)
	uri := "scout://sessions/scout/turn/7/conv/4/response/" + digest
	if args := query.lastArgs(qSessionActiveTurn); !reflect.DeepEqual(args, []any{int64(7), "conv"}) {
		t.Fatalf("active turn args = %v", args)
	}
	if args := query.lastArgs(qSessionCompleteTurn); !reflect.DeepEqual(args, []any{uri, digest, int64(7), "conv", int64(4), int64(6)}) {
		t.Fatalf("complete args = %v", args)
	}

	// Snapshot moved on before the upload: conflict, nothing uploaded.
	stale := &fake.ObjectStorage{}
	err := newDurableSessionStore(&persistenceQueryFake{rows: map[string][][]any{qSessionActiveTurn: {{int64(4), int64(7)}}}}, stale).
		Complete(context.Background(), 7, "conv", 6, domain.TurnResult{Response: response})
	if !errors.Is(err, domain.ErrRevisionConflict) || len(stale.Uploads) != 0 {
		t.Fatalf("stale = %v, uploads = %v", err, stale.Uploads)
	}
	// Turn changed between upload and update: conflict, orphan removed.
	raced := &fake.ObjectStorage{}
	err = newDurableSessionStore(&persistenceQueryFake{rows: map[string][][]any{qSessionActiveTurn: {{int64(4), int64(6)}}}}, raced).
		Complete(context.Background(), 7, "conv", 6, domain.TurnResult{Response: response})
	if !errors.Is(err, domain.ErrRevisionConflict) || len(raced.Deletes) != 1 {
		t.Fatalf("raced = %v, deletes = %v", err, raced.Deletes)
	}
	// No executing turn.
	err = newDurableSessionStore(&persistenceQueryFake{}, &fake.ObjectStorage{}).Complete(context.Background(), 7, "conv", 0, domain.TurnResult{})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no turn = %v", err)
	}
}
