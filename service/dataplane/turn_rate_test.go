package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	keellimiter "github.com/nauticana/keel/limiter"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
	"github.com/nauticana/scout/service/scope"
)

var qReleaseGet = onlyQuery(scope.ReleaseReadQueries)

func onlyQuery(queries map[string]string) string {
	for name := range queries {
		return name
	}
	return ""
}

// frozenRelease is the effective_agent_release row of agent "agent" of tenant 7 at version,
// with a digest the reader verifies.
func frozenRelease(version string, resources ...domain.EffectiveResource) []any {
	release := domain.EffectiveRelease{
		TenantID: 7, AgentID: "agent", AgentVersion: version, ScopeID: "a:agent",
		CompiledAt: time.Unix(1_700_000_000, 0).UTC(), Resources: resources,
	}
	payload, err := json.Marshal(release.Resources)
	if err != nil {
		panic(err)
	}
	return []any{release.ScopeID, string(payload), scope.Digest(release), nil, release.CompiledAt}
}

func turnLimits(value string) domain.EffectiveResource {
	return domain.EffectiveResource{ResourceKind: domain.ResourceLimits, ResourceID: "limits", Value: []byte(value)}
}

// rateLimitedQuery opens turns of a conversation pinned to release "v1", which allows
// turnsPerMinute; rows adds the answers of the test.
func rateLimitedQuery(turnsPerMinute string, rows map[string][][]any) *queueQueryFake {
	query := &queueQueryFake{rows: map[string][][]any{
		qRecordPinnedAgent: {{ingressRequest().AgentID, "v1"}},
		qReleaseGet:        {frozenRelease("v1", turnLimits(`{"turns_per_minute":`+turnsPerMinute+`,"max_steps":0,"max_tool_calls":0,"max_duration_seconds":0}`))},
	}}
	maps.Copy(query.rows, rows)
	return query
}

var rateInput = domain.ObjectRef{URI: "scout://turns/input", Digest: testDigest("input")}

func TestTableTurnRecordStoreRefusesATurnBeyondTheReleaseTurnRate(t *testing.T) {
	query := rateLimitedQuery("2", map[string][][]any{qRecordRecentTurn: {{int64(1500)}}, qRecordOpen: {{int64(9)}}})
	_, err := newTestRecordStore(query).Open(context.Background(), ingressRequest(), rateInput)

	var limit *keellimiter.LimitError
	if !errors.Is(err, domain.ErrRateLimited) || !errors.As(err, &limit) {
		t.Fatalf("error = %v, want a rate limit", err)
	}
	if limit.Scope != "agent.turns_per_minute" || limit.After != 1500*time.Millisecond || limit.ErrorHeaders().Get("Retry-After") != "2" {
		t.Fatalf("limit = %+v, headers = %v", limit, limit.ErrorHeaders())
	}
	// The limits are the conversation's pinned release's, read inside the opening transaction.
	if args := query.firstArgs(qReleaseGet); len(args) != 3 || args[0] != int64(7) || args[1] != "agent" || args[2] != "v1" {
		t.Fatalf("release read args = %v", args)
	}
	// Opens of one agent serialize after the conversation lock, and the second-newest turn decides.
	if locks, rate := query.args[qRecordLock], query.args[qTurnRateLock]; len(locks) != 1 || locks[0][0] != "7:conversation-1" ||
		len(rate) != 1 || rate[0][0] != "turn-rate/7/agent" || indexOf(query.calls, qTurnRateLock) < indexOf(query.calls, qRecordLock) {
		t.Fatalf("locks = %v, rate locks = %v", locks, rate)
	}
	if args := query.firstArgs(qRecordRecentTurn); len(args) != 3 || args[0] != int64(7) || args[1] != "agent" || args[2] != 1 {
		t.Fatalf("recent turn args = %v", args)
	}
	if query.firstArgs(qRecordOpen) != nil {
		t.Fatal("a refused turn must not be opened")
	}
}

func TestTableTurnRecordStoreOpensATurnUnderTheReleaseTurnRate(t *testing.T) {
	query := rateLimitedQuery("2", map[string][][]any{qRecordOpen: {{int64(9)}}})
	turnNo, err := newTestRecordStore(query).Open(context.Background(), ingressRequest(), rateInput)
	if err != nil || turnNo != 9 {
		t.Fatalf("turn = %d, error = %v", turnNo, err)
	}
	if args := query.firstArgs(qTurnRateReplay); len(args) != 2 || args[0] != int64(7) || args[1] != "request-1" {
		t.Fatalf("replay lookup args = %v", args)
	}
}

func TestTableTurnRecordStoreNeverRefusesAReplayedRequest(t *testing.T) {
	// The replay is answered even when its release has lost the frozen row.
	for name, frozen := range map[string]bool{"frozen": true, "unfrozen": false} {
		query := rateLimitedQuery("1", map[string][][]any{
			qRecordRecentTurn: {{int64(400)}},
			qTurnRateReplay:   {{int64(1)}},
			qRecordOpen:       nil,
			qRecordFind:       {{int64(4), "conversation-1", string(domain.TurnQueued), rateInput.Digest, nil, nil, int64(0), ""}},
		})
		if !frozen {
			query.rows[qReleaseGet] = nil
		}
		turnNo, err := newTestRecordStore(query).Open(context.Background(), ingressRequest(), rateInput)
		if err != nil || turnNo != 4 {
			t.Fatalf("%s: turn = %d, error = %v, want the replay answered with its turn", name, turnNo, err)
		}
		if query.firstArgs(qReleaseGet) != nil || query.firstArgs(qTurnRateLock) != nil {
			t.Fatalf("%s: a replay read the release or took the rate lock: %v", name, query.calls)
		}
	}
}

func TestTableTurnRecordStoreSkipsTheRateCheckWithoutATurnLimit(t *testing.T) {
	query := rateLimitedQuery("0", map[string][][]any{qRecordRecentTurn: {{int64(1500)}}, qRecordOpen: {{int64(9)}}})
	if _, err := newTestRecordStore(query).Open(context.Background(), ingressRequest(), rateInput); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if query.firstArgs(qRecordRecentTurn) != nil || query.firstArgs(qTurnRateLock) != nil {
		t.Fatalf("an unlimited agent took the rate lock: calls = %v", query.calls)
	}
}

func TestTableTurnRecordStoreFailsClosedWithoutAnIntactFrozenRelease(t *testing.T) {
	tampered := frozenRelease("v1", turnLimits(`{"turns_per_minute":1,"max_steps":0,"max_tool_calls":0,"max_duration_seconds":0}`))
	tampered[2] = testDigest("another release")
	for name, release := range map[string][][]any{"missing": nil, "tampered": {tampered}} {
		query := rateLimitedQuery("1", map[string][][]any{qReleaseGet: release, qRecordOpen: {{int64(9)}}})
		_, err := newTestRecordStore(query).Open(context.Background(), ingressRequest(), rateInput)
		if err == nil || query.firstArgs(qRecordOpen) != nil {
			t.Fatalf("%s: error = %v, want the turn refused unopened", name, err)
		}
	}
}

func TestTurnIngressRefusesARateLimitedTurnBeforeItReservesBudget(t *testing.T) {
	recorder := &ingressRecorder{}
	ingress := newTestIngress(t, recorder, nil)
	ingress.Records.(*fake.TurnRecordStore).OpenFunc = func(context.Context, domain.TurnRequest, domain.ObjectRef) (int64, error) {
		recorder.calls = append(recorder.calls, "open")
		return 0, &keellimiter.LimitError{Err: domain.ErrRateLimited, Scope: turnRateScope, After: 3 * time.Second}
	}
	_, err := ingress.OpenTurn(context.Background(), ingressRequest())
	var limit *keellimiter.LimitError
	if !errors.As(err, &limit) || limit.ErrorHeaders().Get("Retry-After") != "3" {
		t.Fatalf("error = %v, want the retry advice kept for the client", err)
	}
	if strings.Join(recorder.calls, ",") != "limit,find,open" {
		t.Fatalf("calls = %v, want nothing reserved, subscribed, or dispatched", recorder.calls)
	}
}

func indexOf(calls []string, name string) int {
	for i, call := range calls {
		if call == name {
			return i
		}
	}
	return -1
}
