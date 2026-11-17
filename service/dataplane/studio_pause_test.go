package dataplane

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type enabledStudio struct {
	contract.AgentStudioHTTPBackend
	agentID, reason string
}

func (studio *enabledStudio) Suspend(_ context.Context, _ domain.StudioActor, agentID, reason string) (domain.AgentEnabledState, error) {
	studio.agentID, studio.reason = agentID, reason
	return domain.AgentEnabledState{DraftRevision: 5}, nil
}

type cancellerFunc func(context.Context, int64, string, string) error

func (function cancellerFunc) Cancel(ctx context.Context, tenantID int64, requestID, reason string) error {
	return function(ctx, tenantID, requestID, reason)
}

func TestStudioPauseDisablesFirstThenCancelsLiveTurns(t *testing.T) {
	studio := &enabledStudio{}
	query := &queueQueryFake{sequence: map[string][][][]any{qPauseLiveTurns: {{{"r-1"}, {"r-2"}}, nil}}}
	var cancelled []string
	pause := &StudioPause{DB: queueDBFake{query: query}, Studio: studio, Canceller: cancellerFunc(func(_ context.Context, _ int64, requestID, _ string) error {
		cancelled = append(cancelled, requestID)
		return nil
	})}
	result, err := pause.Pause(context.Background(), usageActor, domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseCancel, Reason: "incident 12"})
	if err != nil || result.Cancelled != 2 || result.DraftRevision != 5 || len(cancelled) != 2 {
		t.Fatalf("result = %+v, cancelled = %v, error = %v", result, cancelled, err)
	}
	if studio.agentID != "writer-a" || studio.reason != "paused: incident 12" {
		t.Fatalf("suspended %q for %q", studio.agentID, studio.reason)
	}
}

func TestStudioPauseFinishLeavesLiveTurnsAndNeedsAReason(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{}}
	pause := &StudioPause{DB: queueDBFake{query: query}, Studio: &enabledStudio{}, Canceller: cancellerFunc(func(context.Context, int64, string, string) error {
		t.Fatal("finish must not cancel")
		return nil
	})}
	if _, err := pause.Pause(context.Background(), usageActor, domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseFinish, Reason: "quiet hours"}); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if len(query.calls) != 0 {
		t.Fatalf("queries = %v", query.calls)
	}
	if _, err := pause.Pause(context.Background(), usageActor, domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseFinish}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("reasonless pause = %v, want validation", err)
	}
}

func TestStudioPauseStopsWhenATurnIgnoresItsCancellation(t *testing.T) {
	query := &queueQueryFake{rows: map[string][][]any{qPauseLiveTurns: make([][]any, defaultPauseBatch)}}
	for index := range query.rows[qPauseLiveTurns] {
		query.rows[qPauseLiveTurns][index] = []any{"r-stuck"}
	}
	pause := &StudioPause{DB: queueDBFake{query: query}, Studio: &enabledStudio{}, Canceller: cancellerFunc(func(context.Context, int64, string, string) error { return nil })}
	if _, err := pause.Pause(context.Background(), usageActor, domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseCancel, Reason: "incident"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestStudioPauseSkipsATurnThatEndedMeanwhile(t *testing.T) {
	query := &queueQueryFake{sequence: map[string][][][]any{qPauseLiveTurns: {{{"r-done"}, {"r-live"}}, nil}}}
	pause := &StudioPause{DB: queueDBFake{query: query}, Studio: &enabledStudio{}, Canceller: cancellerFunc(func(_ context.Context, _ int64, requestID, _ string) error {
		if requestID == "r-done" {
			return domain.ErrNotFound
		}
		return nil
	})}
	result, err := pause.Pause(context.Background(), usageActor, domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseCancel, Reason: "incident"})
	if err != nil || result.Cancelled != 1 {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
}
