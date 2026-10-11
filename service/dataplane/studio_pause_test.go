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
	request domain.AgentSetEnabledRequest
}

func (studio *enabledStudio) SetEnabled(_ context.Context, _ domain.StudioActor, request domain.AgentSetEnabledRequest) (domain.AgentEnabledState, error) {
	studio.request = request
	return domain.AgentEnabledState{DraftRevision: request.ExpectedDraftRevision + 1}, nil
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
	result, err := pause.Pause(context.Background(), usageActor, domain.AgentPauseRequest{AgentID: "writer-a", Policy: domain.PauseCancel, Reason: "incident 12", ExpectedDraftRevision: 4})
	if err != nil || result.Cancelled != 2 || result.DraftRevision != 5 || len(cancelled) != 2 {
		t.Fatalf("result = %+v, cancelled = %v, error = %v", result, cancelled, err)
	}
	if studio.request.Enabled || studio.request.Reason != "paused: incident 12" {
		t.Fatalf("set enabled = %+v", studio.request)
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
	query := &queueQueryFake{rows: map[string][][]any{qPauseLiveTurns: make([][]any, pauseBatch)}}
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
