package dataplane

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
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

// QueryService binds another catalog to the same fake transaction, as keel's PostgreSQL one does.
func (query *persistenceQueryFake) QueryService(string, map[string]string) keelport.QueryService {
	return query
}
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

// sessionOwner owns turn 4 of conversation conv; the guard rows below say it still does.
var sessionOwner = domain.TurnOwner{TenantID: 7, RequestID: "req-1", Fence: 3}

func newDurableSessionStore(query *persistenceQueryFake, storage *fake.ObjectStorage) *DurableSessionStore {
	if query.rows == nil {
		query.rows = map[string][][]any{}
	}
	if _, set := query.rows[qSessionOwnerGuard]; !set {
		query.rows[qSessionOwnerGuard] = [][]any{{int64(4)}}
	}
	return &DurableSessionStore{DB: persistenceDBFake{query: query}, Objects: newObjectStateStore(storage),
		Settlement: TurnSettlementWriter{Budget: &settlerFake{query: query}, UsageCategory: "model_output"}}
}

// settlerFake is a budget settler joined to the ending turn's transaction; it records each
// settlement among that transaction's statements.
type settlerFake struct {
	query *persistenceQueryFake
	calls []string
	err   error
}

func (settler *settlerFake) SettleTurnTx(_ context.Context, tx keelport.TxQueryService, reservation domain.BudgetReservation, usage domain.Usage, refund bool) error {
	settler.calls = append(settler.calls, fmt.Sprintf("%s/%d/%t", reservation.ReservationID, usage.CostMinorUnits, refund))
	if settler.query != nil && tx == keelport.TxQueryService(settler.query) {
		settler.query.calls = append(settler.query.calls, "budget_settle")
	}
	return settler.err
}

// testSettlement settles reservation res-1 of request req-1 at five output tokens and two cents.
var testSettlement = domain.TurnSettlement{
	Reservation: domain.BudgetReservation{TenantID: 7, ReservationID: "res-1", RequestID: "req-1", Currency: "USD"},
	Usage:       domain.Usage{OutputTokens: 5, CostMinorUnits: 2, Currency: "USD"}, SubjectRef: "agent@v1",
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
		qSessionLoad: {{"v3", int64(4), int64(2), ref.URI, ref.Digest, int64(9), "plan", "42"}},
	}}
	store := newDurableSessionStore(query, storage)
	snapshot, err := store.Load(context.Background(), 7, "conv")
	if err != nil {
		t.Fatal(err)
	}
	want := domain.SessionSnapshot{ConversationID: "conv", AgentVersion: "v3", EndUserRef: "42", LatestTurnNo: 4, LatestStepNo: 2,
		LastCompletedStepID: "plan", State: []byte(`{"memory":"x"}`), StateRef: ref, Revision: 9}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot = %+v, want %+v", snapshot, want)
	}
	if args := query.lastArgs(qSessionLoad); !reflect.DeepEqual(args, []any{int64(7), "conv"}) {
		t.Fatalf("load args = %v", args)
	}
	// Tampered state fails closed.
	storage.Overwrite(objectKeyOf(ref), []byte(`{"memory":"y"}`))
	if _, err := store.Load(context.Background(), 7, "conv"); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered load = %v", err)
	}
}

func TestDurableSessionStoreLoadFreshAndMissingConversations(t *testing.T) {
	fresh := newDurableSessionStore(&persistenceQueryFake{rows: map[string][][]any{
		qSessionLoad: {{"v1", nil, nil, nil, nil, nil, nil, nil}},
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
	if err := store.Checkpoint(context.Background(), sessionOwner, 0, checkpoint); err != nil {
		t.Fatal(err)
	}
	digest := DigestBytes(checkpoint.State)
	if args := query.lastArgs(qSessionOwnerGuard); !reflect.DeepEqual(args, []any{int64(7), "req-1", int64(3)}) {
		t.Fatalf("guard args = %v", args)
	}
	uri := "scout://sessions/scout/checkpoint/7/conv/4/2/" + digest
	wantInsert := []any{int64(7), "conv", int64(4), 2, int64(91), "req-1:91", uri, digest, testFingerprint,
		int64(10), int64(5), 1, int64(3), "USD"}
	if args := query.lastArgs(qSessionInsertCheckpoint); !reflect.DeepEqual(args, wantInsert) {
		t.Fatalf("insert args = %v\nwant %v", args, wantInsert)
	}
	if args := query.lastArgs(qSessionCreateSnapshot); !reflect.DeepEqual(args, []any{int64(7), "conv", int64(4), 2, uri, digest}) {
		t.Fatalf("create args = %v", args)
	}
	// The guarded touch of the owner's turn opens the transaction.
	if !reflect.DeepEqual(query.calls, []string{qSessionOwnerGuard, qSessionInsertCheckpoint, qSessionCreateSnapshot}) || query.commits != 1 || query.rollbacks != 0 {
		t.Fatalf("calls = %v, commits = %d, rollbacks = %d", query.calls, query.commits, query.rollbacks)
	}
	if _, ok := storage.Payload("scout/checkpoint/7/conv/4/2/" + digest); !ok {
		t.Fatal("state object missing")
	}

	if err := store.Checkpoint(context.Background(), sessionOwner, 5, checkpoint); err != nil {
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
	err := newDurableSessionStore(query, storage).Checkpoint(context.Background(), sessionOwner, 5, checkpoint)
	if !errors.Is(err, domain.ErrRevisionConflict) || query.rollbacks != 1 || query.commits != 0 {
		t.Fatalf("conflict = %v, rollbacks = %d, commits = %d", err, query.rollbacks, query.commits)
	}
	if len(storage.Deletes) != 0 || len(storage.Objects) != 1 {
		t.Fatalf("shared object was deleted: %v", storage.Deletes)
	}
	// The winning writer committed different content: our upload is an orphan and goes.
	other := &fake.ObjectStorage{}
	query = &persistenceQueryFake{rows: map[string][][]any{qSessionCheckpointDigest: {{strings.Repeat("f", 64)}}}}
	err = newDurableSessionStore(query, other).Checkpoint(context.Background(), sessionOwner, 5, checkpoint)
	if !errors.Is(err, domain.ErrRevisionConflict) || len(other.Deletes) != 1 || len(other.Objects) != 0 {
		t.Fatalf("conflict = %v, deletes = %v", err, other.Deletes)
	}
}

// A checkpoint by an owner the turn moved past writes nothing, and keeps its upload, which the
// current owner may share; the reconciliation sweep reclaims an orphan.
func TestDurableSessionStoreCheckpointRefusesAFencedOutOwner(t *testing.T) {
	storage := &fake.ObjectStorage{}
	query := &persistenceQueryFake{rows: map[string][][]any{qSessionOwnerGuard: nil}}
	err := newDurableSessionStore(query, storage).Checkpoint(context.Background(), sessionOwner, 5, validCheckpoint("x"))
	if !errors.Is(err, domain.ErrFenceLost) || query.rollbacks != 1 || query.commits != 0 || len(storage.Deletes) != 0 {
		t.Fatalf("stale = %v, rollbacks = %d, deletes = %v", err, query.rollbacks, storage.Deletes)
	}
	if !reflect.DeepEqual(query.calls, []string{qSessionOwnerGuard}) {
		t.Fatalf("a fenced-out owner wrote: %v", query.calls)
	}
	// An owner of another turn of the conversation cannot checkpoint this one.
	query = &persistenceQueryFake{rows: map[string][][]any{qSessionOwnerGuard: {{int64(5)}}}}
	if err := newDurableSessionStore(query, storage).Checkpoint(context.Background(), sessionOwner, 5, validCheckpoint("x")); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("other turn = %v", err)
	}
	if err := newDurableSessionStore(&persistenceQueryFake{}, storage).Checkpoint(context.Background(), domain.TurnOwner{}, 5, validCheckpoint("x")); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("missing owner = %v", err)
	}
}

func TestDurableSessionStoreCheckpointFailureDeletesUploadAndJoinsErrors(t *testing.T) {
	dbErr := errors.New("db down")
	deleteErr := errors.New("bucket unreachable")
	storage := &fake.ObjectStorage{DeleteFunc: func(context.Context, string) error { return deleteErr }}
	query := &persistenceQueryFake{errs: map[string]error{qSessionInsertCheckpoint: dbErr}}
	err := newDurableSessionStore(query, storage).Checkpoint(context.Background(), sessionOwner, 5, validCheckpoint("x"))
	if !errors.Is(err, dbErr) || !errors.Is(err, deleteErr) || query.rollbacks != 1 {
		t.Fatalf("error = %v, rollbacks = %d", err, query.rollbacks)
	}
	// Commit failure without a reference check row: object removed best effort.
	clean := &fake.ObjectStorage{}
	query = &persistenceQueryFake{rows: map[string][][]any{qSessionAdvanceSnapshot: {{int64(6)}}}, commitErr: dbErr}
	err = newDurableSessionStore(query, clean).Checkpoint(context.Background(), sessionOwner, 5, validCheckpoint("x"))
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
		if err := store.Checkpoint(context.Background(), sessionOwner, 1, checkpoint); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := store.Checkpoint(context.Background(), sessionOwner, -1, validCheckpoint("x")); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative revision: %v", err)
	}
}

func TestDurableSessionStoreCheckpointAcceptsDehydratedReference(t *testing.T) {
	storage := &fake.ObjectStorage{}
	query := &persistenceQueryFake{rows: map[string][][]any{qSessionAdvanceSnapshot: {{int64(2)}}}}
	checkpoint := validCheckpoint("")
	checkpoint.State = nil
	checkpoint.StateRef = domain.ObjectRef{URI: "scout://sessions/elsewhere", Digest: testFingerprint}
	if err := newDurableSessionStore(query, storage).Checkpoint(context.Background(), sessionOwner, 1, checkpoint); err != nil {
		t.Fatal(err)
	}
	if len(storage.Uploads) != 0 || query.lastArgs(qSessionInsertCheckpoint)[6] != "scout://sessions/elsewhere" {
		t.Fatalf("uploads = %v, args = %v", storage.Uploads, query.lastArgs(qSessionInsertCheckpoint))
	}
}

func TestDurableSessionStoreCompleteGuardsRevisionAndStatus(t *testing.T) {
	storage := &fake.ObjectStorage{}
	query := &persistenceQueryFake{rows: map[string][][]any{
		qSessionActiveTurn:   {{int64(4), int64(6), "req-1"}},
		qSessionCompleteTurn: {{int64(4)}},
	}}
	store := newDurableSessionStore(query, storage)
	response := []byte(`{"text":"done"}`)
	if err := store.Complete(context.Background(), sessionOwner, "conv", 6, domain.TurnResult{Response: response}, testSettlement); err != nil {
		t.Fatal(err)
	}
	digest := DigestBytes(response)
	uri := "scout://sessions/scout/turn/7/conv/4/response/" + digest
	if args := query.lastArgs(qSessionActiveTurn); !reflect.DeepEqual(args, []any{int64(7), "conv"}) {
		t.Fatalf("active turn args = %v", args)
	}
	want := []any{uri, digest, int64(7), "conv", int64(4), "req-1", int64(3), int64(6), "json", string(response)}
	if args := query.lastArgs(qSessionCompleteTurn); !reflect.DeepEqual(args, want) {
		t.Fatalf("complete args = %v", args)
	}

	// Snapshot moved on before the upload: conflict, nothing uploaded.
	stale := &fake.ObjectStorage{}
	err := newDurableSessionStore(&persistenceQueryFake{rows: map[string][][]any{qSessionActiveTurn: {{int64(4), int64(7), "req-1"}}}}, stale).
		Complete(context.Background(), sessionOwner, "conv", 6, domain.TurnResult{Response: response}, testSettlement)
	if !errors.Is(err, domain.ErrRevisionConflict) || len(stale.Uploads) != 0 {
		t.Fatalf("stale = %v, uploads = %v", err, stale.Uploads)
	}
	// Turn changed between upload and update while its fence held: conflict, orphan removed.
	raced := &fake.ObjectStorage{}
	err = newDurableSessionStore(&persistenceQueryFake{rows: map[string][][]any{
		qSessionActiveTurn: {{int64(4), int64(6), "req-1"}}, qSessionTurnFence: {{int64(3), string(domain.TurnRunning)}},
	}}, raced).Complete(context.Background(), sessionOwner, "conv", 6, domain.TurnResult{Response: response}, testSettlement)
	if !errors.Is(err, domain.ErrRevisionConflict) || len(raced.Deletes) != 1 {
		t.Fatalf("raced = %v, deletes = %v", err, raced.Deletes)
	}
	// No executing turn.
	err = newDurableSessionStore(&persistenceQueryFake{}, &fake.ObjectStorage{}).Complete(context.Background(), sessionOwner, "conv", 0, domain.TurnResult{}, testSettlement)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("no turn = %v", err)
	}
}

// Completion is refused to an owner the turn moved past, and to one whose turn is not the
// conversation's executing turn, without touching the upload the current owner may share.
func TestDurableSessionStoreCompleteRefusesAFencedOutOwner(t *testing.T) {
	response := []byte(`{"text":"done"}`)
	held := &fake.ObjectStorage{}
	query := &persistenceQueryFake{rows: map[string][][]any{
		qSessionActiveTurn: {{int64(4), int64(6), "req-1"}}, qSessionTurnFence: {{int64(4), string(domain.TurnHeld)}},
	}}
	err := newDurableSessionStore(query, held).Complete(context.Background(), sessionOwner, "conv", 6, domain.TurnResult{Response: response}, testSettlement)
	if !errors.Is(err, domain.ErrFenceLost) || len(held.Deletes) != 0 {
		t.Fatalf("held = %v, deletes = %v", err, held.Deletes)
	}
	// An earlier turn still live, parked or held, is the executing turn: a later one cannot complete.
	query = &persistenceQueryFake{rows: map[string][][]any{qSessionActiveTurn: {{int64(3), int64(6), "req-0"}}}}
	err = newDurableSessionStore(query, &fake.ObjectStorage{}).Complete(context.Background(), sessionOwner, "conv", 6, domain.TurnResult{Response: response}, testSettlement)
	if !errors.Is(err, domain.ErrFenceLost) || slices.Contains(query.calls, qSessionCompleteTurn) {
		t.Fatalf("out of order = %v, calls = %v", err, query.calls)
	}
	if !strings.Contains(durableSessionQueries[qSessionActiveTurn], sqlTurnHeld) || !strings.Contains(durableSessionQueries[qSessionActiveTurn], sqlTurnSuspended) {
		t.Fatal("the executing turn must be chosen among every live turn, suspended and held included")
	}
}

func TestResultCardKeepsJSONWrapsTextAndReferencesWhatIsTooLarge(t *testing.T) {
	ref := domain.ObjectRef{URI: "scout://r", Digest: "d"}
	for name, tc := range map[string]struct {
		response   []byte
		kind, card string
	}{
		"json":      {[]byte(`{"a":1}`), "json", `{"a":1}`},
		"text":      {[]byte("hello"), "text", `{"text":"hello"}`},
		"too large": {make([]byte, defaultResultCardBytes+1), "reference", `{"response_digest":"d","response_uri":"scout://r"}`},
	} {
		if kind, card := resultCard(tc.response, ref, defaultResultCardBytes); kind != tc.kind || card != tc.card {
			t.Errorf("%s: %s %s", name, kind, card)
		}
	}
}

func TestDurableSessionStoreResultCardLimitIsConfigurable(t *testing.T) {
	response := []byte(`{"text":"done"}`)
	complete := func(maxBytes int) (string, error) {
		query := &persistenceQueryFake{rows: map[string][][]any{
			qSessionActiveTurn: {{int64(4), int64(6), "req-1"}}, qSessionCompleteTurn: {{int64(4)}},
		}}
		store := newDurableSessionStore(query, &fake.ObjectStorage{})
		store.MaxResultCardBytes = maxBytes
		err := store.Complete(context.Background(), sessionOwner, "conv", 6, domain.TurnResult{Response: response}, testSettlement)
		if args := query.lastArgs(qSessionCompleteTurn); len(args) > 8 {
			return args[8].(string), err
		}
		return "", err
	}
	if kind, err := complete(0); err != nil || kind != "json" {
		t.Fatalf("zero limit takes the 64 KiB default: kind %q, %v", kind, err)
	}
	if kind, err := complete(len(response) - 1); err != nil || kind != "reference" {
		t.Fatalf("a limit below the response references it: kind %q, %v", kind, err)
	}
	if _, err := complete(-1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("a negative limit is refused, got %v", err)
	}
}
