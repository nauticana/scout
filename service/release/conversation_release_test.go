package release

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func stickyResolver(store *fake.ConversationReleaseStore, platform string) *StickyReleaseResolver {
	return &StickyReleaseResolver{
		Store: store,
		Platform: fake.TenantPlatformReleaseResolverFunc(func(context.Context, int64, string) (string, error) {
			return platform, nil
		}),
		Now: func() time.Time { return testStart },
	}
}

func TestStickyReleaseResolverPersistsBothIdentitiesOnce(t *testing.T) {
	store := &fake.ConversationReleaseStore{}
	resolver := stickyResolver(store, "2026.08.1")
	first, err := resolver.Resolve(context.Background(), 8, "conversation-a", "3")
	if err != nil || first.AgentVersion != "3" || first.PlatformVersion != "2026.08.1" {
		t.Fatalf("first = %+v, err = %v", first, err)
	}
	// A later turn reads the persisted pair even after the live release moved on.
	resolver.Platform = fake.TenantPlatformReleaseResolverFunc(func(context.Context, int64, string) (string, error) {
		return "2026.08.2", nil
	})
	second, err := resolver.Resolve(context.Background(), 8, "conversation-a", "3")
	if err != nil || second != first {
		t.Fatalf("second = %+v, err = %v", second, err)
	}
	if _, err := resolver.Resolve(context.Background(), 8, "conversation-a", "4"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("version switch err = %v", err)
	}
}

func drainFixture(t *testing.T, stage domain.RolloutStage, stageStartedAt time.Time, policy domain.SessionDrainPolicy) (*SessionDrainer, *fake.ConversationReleaseStore, *[]string) {
	t.Helper()
	states := fake.NewRolloutStateStore()
	if err := states.Create(context.Background(), domain.RolloutState{PlatformVersion: "2026.08.1", Stage: stage, StageStartedAt: stageStartedAt}); err != nil {
		t.Fatal(err)
	}
	releases := &fake.ConversationReleaseStore{Releases: map[string]domain.ConversationRelease{
		"8|conversation-a": {TenantID: 8, ConversationID: "conversation-a", AgentVersion: "3", PlatformVersion: "2026.08.1"},
	}}
	cancelled := &[]string{}
	drainer := &SessionDrainer{
		States: states, Releases: releases,
		Platform: fake.TenantPlatformReleaseResolverFunc(func(context.Context, int64, string) (string, error) {
			return "2026.07.9", nil
		}),
		Canceller: fake.TurnCancellerFunc(func(_ context.Context, _ int64, requestID, _ string) error {
			*cancelled = append(*cancelled, requestID)
			return nil
		}),
		Policy: policy,
		Now:    func() time.Time { return testStart },
	}
	return drainer, releases, cancelled
}

func TestSessionDrainerKeepsSessionsInsideWindow(t *testing.T) {
	policy := domain.SessionDrainPolicy{Window: time.Hour}
	drainer, releases, _ := drainFixture(t, domain.StageRolledBack, testStart.Add(-10*time.Minute), policy)
	release, err := drainer.AdmitTurn(context.Background(), releases.Releases["8|conversation-a"])
	if err != nil || release.PlatformVersion != "2026.08.1" {
		t.Fatalf("release = %+v, err = %v", release, err)
	}
}

func TestSessionDrainerMigratesAfterWindowAndOnQuarantine(t *testing.T) {
	policy := domain.SessionDrainPolicy{Window: time.Hour}
	for _, testCase := range []struct {
		name    string
		stage   domain.RolloutStage
		started time.Time
	}{
		{"window elapsed", domain.StageRolledBack, testStart.Add(-2 * time.Hour)},
		{"quarantined", domain.StageQuarantined, testStart},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			drainer, releases, _ := drainFixture(t, testCase.stage, testCase.started, policy)
			release, err := drainer.AdmitTurn(context.Background(), releases.Releases["8|conversation-a"])
			if err != nil || release.PlatformVersion != "2026.07.9" || release.AgentVersion != "3" {
				t.Fatalf("release = %+v, err = %v", release, err)
			}
			if stored := releases.Releases["8|conversation-a"]; stored.PlatformVersion != "2026.07.9" {
				t.Fatalf("stored = %+v", stored)
			}
		})
	}
}

func TestSessionDrainerCancelsRunningTurnOnCriticalSafety(t *testing.T) {
	policy := domain.SessionDrainPolicy{Window: time.Hour, CancelOnCriticalSafety: true}
	drainer, releases, cancelled := drainFixture(t, domain.StageQuarantined, testStart, policy)
	err := drainer.Interrupt(context.Background(), releases.Releases["8|conversation-a"], "request-1")
	if !errors.Is(err, domain.ErrTurnCanceled) {
		t.Fatalf("err = %v", err)
	}
	if len(*cancelled) != 1 || (*cancelled)[0] != "request-1" {
		t.Fatalf("cancelled = %v", *cancelled)
	}
}

func TestSessionDrainerLeavesRunningTurnOnLiveRelease(t *testing.T) {
	policy := domain.SessionDrainPolicy{Window: time.Hour, CancelOnCriticalSafety: true}
	drainer, releases, cancelled := drainFixture(t, domain.StageRolledBack, testStart, policy)
	if err := drainer.Interrupt(context.Background(), releases.Releases["8|conversation-a"], "request-1"); err != nil {
		t.Fatal(err)
	}
	if len(*cancelled) != 0 {
		t.Fatalf("cancelled = %v", *cancelled)
	}
}

func TestTableConversationReleaseStoreArguments(t *testing.T) {
	query := newQueryFake(map[string][][]any{"scout_release_conversation_put": {{"conversation-a"}}})
	store := &TableConversationReleaseStore{DB: dbFake{query: query}}
	release := domain.ConversationRelease{TenantID: 8, ConversationID: "conversation-a", AgentVersion: "3", PlatformVersion: "2026.08.1", ResolvedAt: testStart}
	if err := store.Put(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	requireArgs(t, query, qConversationReleasePut,
		int64(8), "conversation-a", "2026.08.1", testStart,
		int64(8), "conversation-a", "3",
		int64(8), "conversation-a")
}

func TestTableConversationReleaseStoreRejectsSecondIdentity(t *testing.T) {
	query := newQueryFake(map[string][][]any{
		qConversationReleaseGet: {{"3", "2026.08.1", testStart}},
	})
	store := &TableConversationReleaseStore{DB: dbFake{query: query}}
	err := store.Put(context.Background(), domain.ConversationRelease{
		TenantID: 8, ConversationID: "conversation-a", AgentVersion: "3", PlatformVersion: "2026.08.2", ResolvedAt: testStart,
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestRingPlatformReleaseResolverPrefersRingStage(t *testing.T) {
	query := newQueryFake(map[string][][]any{
		qTenantRingOrder: {{int64(2)}},
		qLiveReleaseCandidates: {
			{"2026.08.1", "tenant_canary", int64(100), int64(2)},
			{"2026.07.9", "global_default", int64(100), int64(0)},
		},
	})
	resolver := &RingPlatformReleaseResolver{DB: dbFake{query: query}}
	version, err := resolver.Current(context.Background(), 8, "conversation-a")
	if err != nil || version != "2026.08.1" {
		t.Fatalf("version = %q, err = %v", version, err)
	}

	// A tenant outside the canary ring stays on the global default.
	query.rows[qTenantRingOrder] = [][]any{{int64(5)}}
	if version, err = resolver.Current(context.Background(), 8, "conversation-a"); err != nil || version != "2026.07.9" {
		t.Fatalf("outer ring version = %q, err = %v", version, err)
	}
}
