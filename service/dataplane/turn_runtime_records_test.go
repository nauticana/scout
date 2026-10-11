package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func newTestRecordStore(query *queueQueryFake) *TableTurnRecordStore {
	if _, set := query.rows[qRecordAdmits]; !set {
		query.rows[qRecordAdmits] = [][]any{{true}}
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
	// The conversation is pinned under the same lock, before its first turn row exists.
	if len(query.calls) < 6 || query.calls[0] != qRecordLock || query.calls[1] != qRecordAdmits || query.calls[2] != qRecordPinnedAgent ||
		query.calls[3] != qRecordConversation || query.calls[4] != qRecordPinnedAgent || query.calls[5] != qRecordOpen {
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
		qRecordFind:        {{int64(4), "conversation-1", "queued", testDigest("other"), nil, nil}},
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
		qRecordFind: {{int64(4), "conversation-1", "completed", testDigest("input"), ref.URI, ref.Digest}},
	}}
	store := newTestRecordStore(query)
	store.Objects = codec
	turnNo, status, payload, err := store.Find(context.Background(), 7, "request-1")
	if err != nil || turnNo != 4 || status != "completed" || string(payload) != "answer" {
		t.Fatalf("turn = %d, status = %q, payload = %q, error = %v", turnNo, status, payload, err)
	}
	query.rows[qRecordFind] = nil
	if _, _, _, err := store.Find(context.Background(), 7, "request-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

func TestTableTurnRecordStoreFailAndUsageArgumentOrder(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qRecordFail: {{int64(4)}}, qLedgerInsertUsageEvent: {{int64(1)}}}}
	store := newTestRecordStore(query)
	if err := store.Fail(context.Background(), 7, "request-1", "failed", "dispatch_failed"); err != nil {
		t.Fatal(err)
	}
	failArgs := query.firstArgs(qRecordFail)
	if len(failArgs) != 6 || failArgs[0] != "failed" || failArgs[3] != int64(7) || failArgs[4] != "request-1" || failArgs[5] != "dispatch_failed" {
		t.Fatalf("fail args = %v", failArgs)
	}
	if err := store.Fail(context.Background(), 7, "request-1", "completed", "x"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
	usage := domain.Usage{InputTokens: 3, OutputTokens: 5, ToolCalls: 1, SearchQueries: 6, CostMinorUnits: 2, Currency: "USD"}
	if err := store.RecordUsage(context.Background(), 7, "conversation-1", 4, "agent@v1", domain.UsageAttribution{
		Principal: domain.PrincipalRef{Kind: domain.PrincipalAgent, ID: "agent"}, ScopeID: "unit",
		OnBehalfOf: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}, WorkRef: "task-9",
	}, usage); err != nil {
		t.Fatal(err)
	}
	usageArgs := query.firstArgs(qLedgerInsertUsageEvent)
	want := []any{int64(7), "conversation-1", int64(4), "model_output", "agent@v1", "agent", "agent", "unit", "human", "42", "task-9",
		int64(3), int64(5), 1, int64(6), int64(2), "USD"}
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
		qRecordOpen:        {{int64(1)}},
	}, sequence: map[string][][][]any{qRecordPinnedAgent: {nil}}}
	store := newTestRecordStore(query)
	store.Versions = explainerFunc(func(context.Context, int64, string, string) (domain.AgentVersionResolution, error) {
		return domain.AgentVersionResolution{Version: "5", Source: domain.VersionFromCanary}, nil
	})
	if _, err := store.Open(context.Background(), ingressRequest(), domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if pin := query.firstArgs(qRecordConversation); pin[1] != "5" || pin[2] != "canary" {
		t.Fatalf("pin args = %v", pin)
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

type explainerFunc func(context.Context, int64, string, string) (domain.AgentVersionResolution, error)

func (function explainerFunc) ExplainVersion(ctx context.Context, tenantID int64, agentID, conversationID string) (domain.AgentVersionResolution, error) {
	return function(ctx, tenantID, agentID, conversationID)
}

func TestTableTurnRecordStoreLateFailKeepsTheTerminalPayload(t *testing.T) {
	storage := &fake.ObjectStorage{Name: "turns"}
	query := &queueQueryFake{rows: map[string][][]any{}}
	store := newTestRecordStore(query)
	store.Objects = &ObjectStateStore{Storage: storage, MaxBytes: 1 << 20}
	if err := store.Fail(context.Background(), 7, "request-1", "failed", "model_error"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("late fail = %v, want conflict", err)
	}
	first := query.firstArgs(qRecordFail)
	if err := store.Fail(context.Background(), 7, "request-1", "failed", "rejected"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second late fail = %v, want conflict", err)
	}
	second := query.args[qRecordFail][1]
	if first[1] == second[1] {
		t.Fatalf("two failure codes share object %v", first[1])
	}
}
