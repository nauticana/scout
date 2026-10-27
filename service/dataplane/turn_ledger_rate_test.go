package dataplane

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	keellimiter "github.com/nauticana/keel/limiter"

	"github.com/nauticana/scout/domain"
)

var ledgerRelease = domain.AgentReleaseReference{AgentID: "agent", Version: "v1", Digest: testDigest("release")}

// ledgerRateQuery answers a TurnLedger.Begin whose workspace conversation is "conversation-1"
// and whose release "v1" allows turnsPerMinute.
func ledgerRateQuery(turnsPerMinute string, rows map[string][][]any) *queueQueryFake {
	query := &queueQueryFake{rows: map[string][][]any{
		qLedgerGetConversation: {{"conversation-1"}},
		qReleaseGet:            {frozenRelease("v1", turnLimits(`{"turns_per_minute":`+turnsPerMinute+`,"max_steps":0,"max_tool_calls":0,"max_duration_seconds":0}`))},
	}}
	maps.Copy(query.rows, rows)
	return query
}

func beginLedgerTurn(query *queueQueryFake) (TurnState, bool, error) {
	ledger := &TurnLedger{DB: queueDBFake{query: query}, Namespace: "ledger"}
	return ledger.Begin(context.Background(), 7, "user", "request-1", "task", "input", testDigest("input"), domain.TurnRunning, ledgerRelease)
}

func TestTurnLedgerBeginRefusesATurnBeyondTheReleaseTurnRate(t *testing.T) {
	query := ledgerRateQuery("3", map[string][][]any{qRecordRecentTurn: {{int64(2500)}}, qLedgerInsertTurn: {{int64(4)}}})
	_, _, err := beginLedgerTurn(query)

	var limit *keellimiter.LimitError
	if !errors.Is(err, domain.ErrRateLimited) || !errors.As(err, &limit) {
		t.Fatalf("error = %v, want a rate limit", err)
	}
	if limit.Scope != turnRateScope || limit.After != 2500*time.Millisecond || limit.ErrorHeaders().Get("Retry-After") != "3" {
		t.Fatalf("limit = %+v, headers = %v", limit, limit.ErrorHeaders())
	}
	if query.firstArgs(qLedgerInsertTurn) != nil || query.firstArgs(qLedgerInsertDetail) != nil {
		t.Fatal("a refused turn must not be inserted")
	}
	// The release is the ledger's own, read after the conversation lock in the same transaction.
	if args := query.firstArgs(qReleaseGet); len(args) != 3 || args[0] != int64(7) || args[1] != "agent" || args[2] != "v1" {
		t.Fatalf("release read args = %v", args)
	}
	if args := query.firstArgs(qRecordRecentTurn); len(args) != 3 || args[0] != int64(7) || args[1] != "agent" || args[2] != 2 {
		t.Fatalf("recent turn args = %v", args)
	}
	locks := query.args[qLedgerLock]
	if len(locks) != 2 || locks[1][0] != "7:conversation-1" || indexOf(query.calls, qTurnRateReplay) < lastIndexOf(query.calls, qLedgerLock) {
		t.Fatalf("calls = %v, want the rate checked under the conversation lock", query.calls)
	}
	if rate := query.firstArgs(qTurnRateLock); len(rate) != 1 || rate[0] != "turn-rate/7/agent" {
		t.Fatalf("rate lock = %v", rate)
	}
}

func TestTurnLedgerBeginOpensATurnUnderTheReleaseTurnRate(t *testing.T) {
	query := ledgerRateQuery("3", map[string][][]any{
		qLedgerInsertTurn: {{int64(4)}},
		qLedgerFindTurn: {{int64(7), "conversation-1", int64(4), "request-1", string(domain.TurnRunning), testDigest("input"), nil,
			"task", "input", nil, nil, nil, nil, nil, ledgerRelease.Digest, nil, nil, nil, nil, nil, nil, nil, nil, nil, false,
			"agent", "v1", time.Unix(1_700_000_000, 0).UTC(), nil}},
	})
	state, created, err := beginLedgerTurn(query)
	if err != nil || !created || state.TurnNo != 4 {
		t.Fatalf("state = %+v, created = %v, error = %v", state, created, err)
	}
	if indexOf(query.calls, qRecordRecentTurn) < 0 || indexOf(query.calls, qLedgerInsertTurn) < indexOf(query.calls, qRecordRecentTurn) {
		t.Fatalf("calls = %v, want the rate checked before the insert", query.calls)
	}
}

func TestTurnLedgerBeginFailsClosedWithoutAFrozenRelease(t *testing.T) {
	query := ledgerRateQuery("0", map[string][][]any{qReleaseGet: nil, qLedgerInsertTurn: {{int64(4)}}})
	if _, _, err := beginLedgerTurn(query); !errors.Is(err, domain.ErrNotFound) || query.firstArgs(qLedgerInsertTurn) != nil {
		t.Fatalf("error = %v, want the turn refused unopened", err)
	}
}

func lastIndexOf(calls []string, name string) int {
	for i, call := range slices.Backward(calls) {
		if call == name {
			return i
		}
	}
	return -1
}
