package dataplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// recordOwner owns turn 4 of conversation-1; the default guard row says it still does.
var recordOwner = domain.TurnOwner{TenantID: 7, RequestID: "request-1", Fence: 3}

func newTestRecordStore(query *queueQueryFake) *TableTurnRecordStore {
	if _, set := query.rows[qRecordOwnerGuard]; !set {
		query.rows[qRecordOwnerGuard] = [][]any{{int64(4)}}
	}
	if _, set := query.rows[qRecordAdmits]; !set {
		query.rows[qRecordAdmits] = [][]any{{true}}
	}
	if _, set := query.rows[qReleaseGet]; !set {
		query.rows[qReleaseGet] = [][]any{frozenRelease("v1")}
	}
	return &TableTurnRecordStore{
		DB:            queueDBFake{query: query},
		Objects:       &ObjectStateStore{Storage: &fake.ObjectStorage{Name: "turns"}, MaxBytes: 1 << 20},
		UsageCategory: "model_output",
	}
}

func TestTableTurnRecordStoreOpensUnderConversationLock(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{
		qRecordPinnedAgent: {{ingressRequest().AgentID, "v1"}},
		qRecordOpen:        {{int64(4)}},
	}, sequence: map[string][][][]any{qRecordPinnedAgent: {nil}}}
	store := newTestRecordStore(query)
	input := domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}
	turnNo, err := store.Open(context.Background(), ingressRequest(), input)
	if err != nil || turnNo != 4 {
		t.Fatalf("turn = %d, error = %v", turnNo, err)
	}
	// The conversation is pinned under the same lock, before its first turn row exists, and
	// its release's limits are read in the same transaction.
	if len(query.calls) < 8 || query.calls[0] != qRecordLock || query.calls[1] != qRecordAdmits || query.calls[2] != qRecordPinnedAgent ||
		query.calls[3] != qRecordConversation || query.calls[4] != qRecordPinnedAgent || query.calls[5] != qTurnRateReplay ||
		query.calls[6] != qReleaseGet || query.calls[7] != qRecordOpen {
		t.Fatalf("calls = %v", query.calls)
	}
	if pin := query.firstArgs(qRecordConversation); pin[0] != "conversation-1" || pin[2] != "stable" || pin[6] != int64(7) || pin[7] != ingressRequest().AgentID {
		t.Fatalf("conversation pin args = %v", pin)
	}
	args := query.firstArgs(qRecordOpen)
	want := []any{int64(7), "conversation-1", "request-1", input.URI, input.Digest, int64(7), "conversation-1"}
	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}
	for i, value := range want {
		if args[i] != value {
			t.Fatalf("arg %d = %v, want %v", i, args[i], value)
		}
	}
}

func TestTableTurnRecordStoreDetectsReusedRequestID(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{
		qRecordPinnedAgent: {{ingressRequest().AgentID, "v1"}},
		qRecordOpen:        nil,
		qRecordFind:        {{int64(4), "conversation-1", string(domain.TurnQueued), testDigest("other"), nil, nil, int64(0)}},
	}}
	store := newTestRecordStore(query)
	input := domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}
	if _, err := store.Open(context.Background(), ingressRequest(), input); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestTableTurnRecordStoreFindHydratesTerminalPayload(t *testing.T) {
	storage := &fake.ObjectStorage{Name: "turns"}
	codec := &ObjectStateStore{Storage: storage, MaxBytes: 1 << 20}
	ref, err := codec.Dehydrate(context.Background(), "turn-response/7/request-1", []byte("answer"))
	if err != nil {
		t.Fatal(err)
	}
	query := &queueQueryFake{rows: map[string][][]any{
		qRecordFind: {{int64(4), "conversation-1", string(domain.TurnCompleted), testDigest("input"), ref.URI, ref.Digest, int64(6)}},
	}}
	store := newTestRecordStore(query)
	store.Objects = codec
	record, err := store.Find(context.Background(), 7, "request-1")
	if err != nil || record.TurnNo != 4 || record.Status != domain.TurnCompleted || record.Fence != 6 || string(record.Payload) != "answer" {
		t.Fatalf("record = %+v, error = %v", record, err)
	}
	query.rows[qRecordFind] = nil
	if _, err := store.Find(context.Background(), 7, "request-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

func TestTableTurnRecordStoreFailAndUsageArgumentOrder(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qRecordFail: {{int64(4)}}, qLedgerInsertUsageEvent: {{int64(1)}}}}
	store := newTestRecordStore(query)
	if err := store.Fail(context.Background(), recordOwner, domain.TurnFailed, "model_error"); err != nil {
		t.Fatal(err)
	}
	failArgs := query.firstArgs(qRecordFail)
	if len(failArgs) != 7 || failArgs[0] != string(domain.TurnFailed) || failArgs[3] != int64(7) || failArgs[4] != "request-1" ||
		failArgs[5] != int64(3) || failArgs[6] != "model_error" {
		t.Fatalf("fail args = %v", failArgs)
	}
	if err := store.Fail(context.Background(), recordOwner, domain.TurnCompleted, "x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
	if err := store.Fail(context.Background(), domain.TurnOwner{}, domain.TurnFailed, "x"); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("missing owner = %v, want ErrFenceLost", err)
	}
	usage := domain.Usage{InputTokens: 3, OutputTokens: 5, ToolCalls: 1, SearchQueries: 6, CostMinorUnits: 2, Currency: "USD"}
	if err := store.RecordUsage(context.Background(), recordOwner, "conversation-1", 4, "agent@v1", domain.UsageAttribution{
		Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "agent"}, ScopeID: "unit",
		OnBehalfOf: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}, WorkRef: "task-9",
	}, usage); err != nil {
		t.Fatal(err)
	}
	usageArgs := query.firstArgs(qLedgerInsertUsageEvent)
	want := []any{int64(7), "conversation-1", int64(4), "model_output", "agent@v1", "agent", "agent", "unit", "human", "42", "task-9",
		int64(3), int64(5), 1, int64(6), int64(2), "USD", nil, nil}
	if len(usageArgs) != len(want) {
		t.Fatalf("usage args = %v", usageArgs)
	}
	for i, value := range want {
		if usageArgs[i] != value {
			t.Fatalf("usage arg %d = %v, want %v", i, usageArgs[i], value)
		}
	}
}

func testDigest(payload string) string { return DigestBytes([]byte(payload)) }

func TestTableTurnRecordStoreWritesTheHistoryRowForANewTurn(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{
		qRecordPinnedAgent: {{ingressRequest().AgentID, "v1"}},
		qRecordOpen:        {{int64(4)}},
	}}
	store := newTestRecordStore(query)
	store.TaskKind = "agent_turn"
	request := ingressRequest()
	request.Input = []byte("audit the homepage")
	if _, err := store.Open(context.Background(), request, domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	detail := query.firstArgs(qRecordDetail)
	if len(detail) != 5 || detail[0] != int64(4) || detail[1] != "agent_turn" || detail[2] != "audit the homepage" {
		t.Fatalf("history row args = %v", detail)
	}
}

func TestTableTurnRecordStoreRejectsConversationPinnedToAnotherAgent(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qRecordPinnedAgent: {{"other-agent", "v1"}}}}
	store := newTestRecordStore(query)
	input := domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}
	if _, err := store.Open(context.Background(), ingressRequest(), input); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if query.firstArgs(qRecordOpen) != nil {
		t.Fatal("a turn must not be opened against another agent's conversation")
	}
}

func TestTableTurnRecordStoreRequiresADeployedVersionForANewConversation(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qRecordPinnedAgent: nil}}
	store := newTestRecordStore(query)
	input := domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}
	if _, err := store.Open(context.Background(), ingressRequest(), input); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("error = %v, want not ready", err)
	}
}

func TestTableTurnRecordStoreRefusesAnAgentThatTakesNoWork(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qRecordAdmits: {{false}}}}
	store := newTestRecordStore(query)
	input := domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}
	if _, err := store.Open(context.Background(), ingressRequest(), input); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if query.firstArgs(qRecordOpen) != nil {
		t.Fatal("a disabled agent must not open a turn")
	}
}

func TestTableTurnRecordStorePinsOnlyAKnownRelease(t *testing.T) {
	request := ingressRequest()
	request.Release, request.ReleaseReason = "2", "regression check"
	input := domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}
	query := &queueQueryFake{rows: map[string][][]any{}}
	if _, err := newTestRecordStore(query).Open(context.Background(), request, input); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown release = %v, want not found", err)
	}
	query = &queueQueryFake{rows: map[string][][]any{
		qRecordRelease:     {{int64(1)}},
		qRecordPinnedAgent: {{request.AgentID, "3"}},
	}}
	if _, err := newTestRecordStore(query).Open(context.Background(), request, input); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("conversation on another release = %v, want conflict", err)
	}
	query = &queueQueryFake{rows: map[string][][]any{
		qRecordRelease:     {{int64(1)}},
		qRecordPinnedAgent: {{request.AgentID, "2"}},
		qReleaseGet:        {frozenRelease("2")},
		qRecordOpen:        {{int64(1)}},
	}, sequence: map[string][][][]any{qRecordPinnedAgent: {nil}}}
	if _, err := newTestRecordStore(query).Open(context.Background(), request, input); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if pin := query.firstArgs(qRecordConversation); pin[1] != "2" || pin[2] != "request_pin" || pin[5] != "regression check" {
		t.Fatalf("pin args = %v", pin)
	}
}

func TestTableTurnRecordStoreTakesTheTrafficPolicysRelease(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{
		qRecordPinnedAgent: {{ingressRequest().AgentID, "5"}},
		qReleaseGet:        {frozenRelease("5")},
		qRecordOpen:        {{int64(1)}},
	}, sequence: map[string][][][]any{qRecordPinnedAgent: {nil}}}
	store := newTestRecordStore(query)
	store.Versions = explainerFunc(func(context.Context, domain.VersionRequest) (domain.AgentVersionResolution, error) {
		return domain.AgentVersionResolution{Version: "5", Source: domain.VersionFromCanary}, nil
	})
	if _, err := store.Open(context.Background(), ingressRequest(), domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if pin := query.firstArgs(qRecordConversation); pin[1] != "5" || pin[2] != "canary" {
		t.Fatalf("pin args = %v", pin)
	}
}

func TestTableTurnRecordStoreResolvesForTheTrustedSubject(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{
		qRecordPinnedAgent: {{ingressRequest().AgentID, "6"}},
		qReleaseGet:        {frozenRelease("6")},
		qRecordOpen:        {{int64(1)}},
	}, sequence: map[string][][][]any{qRecordPinnedAgent: {nil}}}
	request := ingressRequest()
	request.OnBehalfOf = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}
	var asked domain.VersionRequest
	store := newTestRecordStore(query)
	store.Versions = explainerFunc(func(_ context.Context, versionRequest domain.VersionRequest) (domain.AgentVersionResolution, error) {
		asked = versionRequest
		return domain.AgentVersionResolution{Version: "6", Source: domain.VersionFromPilot,
			Pilot: domain.PilotSubject{Kind: domain.PilotPrincipal, ID: "42"}}, nil
	})
	if _, err := store.Open(context.Background(), request, domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := domain.VersionRequest{TenantID: 7, AgentID: "agent", ConversationID: "conversation-1", Subject: request.OnBehalfOf}
	if asked != want {
		t.Fatalf("version request = %+v, want %+v", asked, want)
	}
	if pin := query.firstArgs(qRecordConversation); pin[1] != "6" || pin[2] != "pilot" || pin[3] != "42" {
		t.Fatalf("pin args = %v", pin)
	}
}

func TestTableTurnRecordStoreFailsWhenTheReleaseCannotBeChosen(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{}, sequence: map[string][][][]any{qRecordPinnedAgent: {nil}}}
	store := newTestRecordStore(query)
	store.Versions = explainerFunc(func(context.Context, domain.VersionRequest) (domain.AgentVersionResolution, error) {
		return domain.AgentVersionResolution{}, domain.ErrCircuitOpen
	})
	if _, err := store.Open(context.Background(), ingressRequest(), domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}); !errors.Is(err, domain.ErrCircuitOpen) {
		t.Fatalf("error = %v, want the resolver's failure", err)
	}
	if query.firstArgs(qRecordConversation) != nil {
		t.Fatal("a conversation must not be pinned when its release cannot be chosen")
	}
}

func TestTableTurnRecordStoreAddressesAnAgentOfTheAliasType(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qRecordAddress: {{"writer-b"}}}}
	agentID, err := newTestRecordStore(query).Address(context.Background(), 7, "writer", "writer-b", "conversation-1")
	if err != nil || agentID != "writer-b" {
		t.Fatalf("agent = %q, error = %v", agentID, err)
	}
	if args := query.firstArgs(qRecordAddress); args[0] != "writer" || args[1] != int64(7) || args[2] != "writer-b" || args[3] != "conversation-1" || args[4] != "writer" {
		t.Fatalf("args = %v", args)
	}
	query = &queueQueryFake{rows: map[string][][]any{}}
	if _, err = newTestRecordStore(query).Address(context.Background(), 7, "writer", "editor-a", "conversation-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other type = %v, want not found", err)
	}
}

type explainerFunc func(context.Context, domain.VersionRequest) (domain.AgentVersionResolution, error)

func (function explainerFunc) ExplainVersion(ctx context.Context, request domain.VersionRequest) (domain.AgentVersionResolution, error) {
	return function(ctx, request)
}

// A late Fail, of a turn that ended or moved to another owner, is refused as a lost fence and
// writes its payload to an object of its own, never over the one the turn's digest names.
func TestTableTurnRecordStoreLateFailKeepsTheTerminalPayload(t *testing.T) {
	storage := &fake.ObjectStorage{Name: "turns"}
	query := &queueQueryFake{rows: map[string][][]any{}}
	store := newTestRecordStore(query)
	store.Objects = &ObjectStateStore{Storage: storage, MaxBytes: 1 << 20}
	if err := store.Fail(context.Background(), recordOwner, domain.TurnFailed, "model_error"); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("late fail = %v, want ErrFenceLost", err)
	}
	first := query.firstArgs(qRecordFail)
	if err := store.Fail(context.Background(), recordOwner, domain.TurnFailed, "rejected"); !errors.Is(err, domain.ErrFenceLost) {
		t.Fatalf("second late fail = %v, want ErrFenceLost", err)
	}
	second := query.args[qRecordFail][1]
	if first[1] == second[1] {
		t.Fatalf("two failure codes share object %v", first[1])
	}
}

// A turn no worker runs, queued or suspended, is failed without an owner; a held one keeps its
// hold and an ended one conflicts.
func TestTableTurnRecordStoreFailsAParkedTurn(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qRecordFailParked: {{int64(4)}}}}
	store := newTestRecordStore(query)
	if err := store.FailParked(context.Background(), 7, "request-1", "dispatch_failed"); err != nil {
		t.Fatal(err)
	}
	if args := query.firstArgs(qRecordFailParked); len(args) != 5 || args[2] != int64(7) || args[3] != "request-1" || args[4] != "dispatch_failed" {
		t.Fatalf("args = %v", args)
	}
	for status, want := range map[domain.TurnStatus]error{domain.TurnHeld: domain.ErrTurnHeld, domain.TurnRunning: domain.ErrConflict} {
		query := &queueQueryFake{rows: map[string][][]any{
			qRecordFind: {{int64(4), "conversation-1", string(status), testDigest("input"), nil, nil, int64(2)}},
		}}
		if err := newTestRecordStore(query).FailParked(context.Background(), 7, "request-1", "rejected"); !errors.Is(err, want) {
			t.Fatalf("%s: %v, want %v", status, err, want)
		}
	}
	if !strings.Contains(turnRecordQueries[qRecordFailParked], sqlTurnQueued+", "+sqlTurnSuspended+")") {
		t.Fatal("only a queued or suspended turn may be failed without an owner")
	}
}

// A hold or suspension yields to a cancellation requested before it: the owner is told its turn
// is cancelled and ends it, so a cancelled turn is never parked out of the canceller's reach.
func TestTableTurnRecordStoreHoldAndSuspendYieldToAPendingCancellation(t *testing.T) {
	for name, transition := range map[string]func(*TableTurnRecordStore) error{
		"hold": func(store *TableTurnRecordStore) error {
			_, err := store.Hold(context.Background(), recordOwner)
			return err
		},
		"suspend": func(store *TableTurnRecordStore) error {
			_, err := store.Suspend(context.Background(), recordOwner, "approval_pending")
			return err
		},
	} {
		cancelled := &queueQueryFake{rows: map[string][][]any{qRecordOwnerState: {{recordOwner.Fence, "R", true, "operator asked"}}}}
		if err := transition(newTestRecordStore(cancelled)); !errors.Is(err, domain.ErrTurnCanceled) || !strings.Contains(err.Error(), "operator asked") {
			t.Fatalf("%s with a pending cancellation = %v, want ErrTurnCanceled", name, err)
		}
		moved := &queueQueryFake{rows: map[string][][]any{qRecordOwnerState: {{recordOwner.Fence + 1, "H", false, ""}}}}
		if err := transition(newTestRecordStore(moved)); !errors.Is(err, domain.ErrFenceLost) {
			t.Fatalf("%s past its fence = %v, want ErrFenceLost", name, err)
		}
	}
	for name, query := range map[string]string{"hold": turnRecordQueries[qRecordHold], "suspend": turnRecordQueries[qRecordSuspend]} {
		if !strings.Contains(query, "cancel_requested_at IS NULL") {
			t.Errorf("%s does not yield to a pending cancellation:\n%s", name, query)
		}
	}
}
