package release

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
)

func TestTableRolloutStateStoreTransitionArguments(t *testing.T) {
	query := newQueryFake(map[string][][]any{qRolloutCasState: {{int64(3)}}})
	store := &TableRolloutStateStore{DB: dbFake{query: query}, Now: func() time.Time { return testStart }}
	next := domain.RolloutState{
		PlatformVersion: "2026.08.1", Stage: domain.StageRegionalRamp, Ring: "regional", TrafficPercentage: 50,
		Generation: 2, StageStartedAt: testStart, MinSamples: 20_000, MinDuration: time.Hour, LeaseOwner: "controller-a",
	}
	transition := domain.RolloutTransition{
		PlatformVersion: "2026.08.1", From: domain.StageTenantCanary, To: domain.StageRegionalRamp,
		FromGeneration: 2, Actor: "rollout-controller", Reason: "healthy", OccurredAt: testStart,
	}
	if err := store.Transition(context.Background(), transition, next); err != nil {
		t.Fatal(err)
	}
	requireArgs(t, query, qRolloutCasState,
		"regional_ramp", "regional", 50, false, nil, testStart, int64(20_000), int64(3_600_000),
		0, 0, nil, "2026.08.1", int64(2), "controller-a", testStart)
	requireArgs(t, query, qRolloutInsertTransition,
		"2026.08.1", "tenant_canary", "regional_ramp", int64(2), "rollout-controller", "healthy", testStart)
	requireArgs(t, query, qRolloutSetRing, 50, "active", nil, "2026.08.1", "regional")
}

func TestTableRolloutStateStoreTransitionLosesStaleCAS(t *testing.T) {
	query := newQueryFake(nil)
	store := &TableRolloutStateStore{DB: dbFake{query: query}, Now: func() time.Time { return testStart }}
	err := store.Transition(context.Background(),
		domain.RolloutTransition{PlatformVersion: "2026.08.1", FromGeneration: 2, OccurredAt: testStart},
		domain.RolloutState{PlatformVersion: "2026.08.1", Stage: domain.StageRegionalRamp, LeaseOwner: "controller-a"})
	if !errors.Is(err, domain.ErrRevisionConflict) {
		t.Fatalf("err = %v", err)
	}
	if _, recorded := query.args[qRolloutInsertTransition]; recorded {
		t.Fatal("a lost CAS still recorded a transition")
	}
}

func TestTableRolloutStateStoreRollbackFinishesRings(t *testing.T) {
	query := newQueryFake(map[string][][]any{qRolloutCasState: {{int64(3)}}})
	store := &TableRolloutStateStore{DB: dbFake{query: query}, Now: func() time.Time { return testStart }}
	err := store.Transition(context.Background(),
		domain.RolloutTransition{PlatformVersion: "2026.08.1", From: domain.StageRegionalRamp, To: domain.StageRolledBack, FromGeneration: 2, Reason: "hard breach", OccurredAt: testStart},
		domain.RolloutState{PlatformVersion: "2026.08.1", Stage: domain.StageRolledBack, LeaseOwner: "controller-a"})
	if err != nil {
		t.Fatal(err)
	}
	requireArgs(t, query, qRolloutFinishRings, "rolled_back", testStart, "hard breach", "2026.08.1")
}

func TestTableRolloutStateStoreLease(t *testing.T) {
	query := newQueryFake(map[string][][]any{qRolloutAcquireLease: {{"controller-a"}}})
	store := &TableRolloutStateStore{DB: dbFake{query: query}, Now: func() time.Time { return testStart }}
	acquired, err := store.Acquire(context.Background(), "2026.08.1", "controller-a", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("acquired = %t, err = %v", acquired, err)
	}
	requireArgs(t, query, qRolloutAcquireLease, "controller-a", testStart.Add(time.Minute), "2026.08.1", "controller-a", testStart)

	query.rows[qRolloutAcquireLease] = nil
	if acquired, err = store.Acquire(context.Background(), "2026.08.1", "controller-b", time.Minute); err != nil || acquired {
		t.Fatalf("held lease acquired = %t, err = %v", acquired, err)
	}
	if _, err = store.Acquire(context.Background(), "2026.08.1", "", time.Minute); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty owner err = %v", err)
	}
}

func TestTableRolloutStateStoreGetDecodesRow(t *testing.T) {
	query := newQueryFake(map[string][][]any{qRolloutGetState: {{
		"2026.08.1", "tenant_canary", "canary", int64(10), int64(4), true, "operator hold", testStart,
		int64(5_000), int64(21_600_000), int64(1), int64(0), testStart, "controller-a", testStart.Add(time.Minute),
	}}})
	store := &TableRolloutStateStore{DB: dbFake{query: query}}
	state, err := store.Get(context.Background(), "2026.08.1")
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != domain.StageTenantCanary || state.Generation != 4 || !state.Paused ||
		state.MinDuration != 6*time.Hour || state.ConsecutiveBreaches != 1 || state.LeaseOwner != "controller-a" {
		t.Fatalf("state = %+v", state)
	}

	query.rows[qRolloutGetState] = nil
	if _, err = store.Get(context.Background(), "2026.08.1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
