package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

type runsBackend struct {
	trace func(context.Context, domain.StudioActor, string, int64) (domain.RunTrace, error)
}

func (runsBackend) Runs(context.Context, domain.StudioActor, domain.AgentRunFilter) ([]domain.AgentRun, error) {
	return nil, nil
}

func (backend runsBackend) Trace(ctx context.Context, actor domain.StudioActor, agentID string, runID int64) (domain.RunTrace, error) {
	return backend.trace(ctx, actor, agentID, runID)
}

func serveRuns(handler *StudioRunsHandler, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	path, _, _ := strings.Cut(target, "?")
	handler.Routes()[path](recorder, authenticatedRequest(http.MethodGet, target, nil))
	return recorder
}

func TestStudioRunTraceRouteServesTheTraceAndBumpsTheFeature(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	handler := &StudioRunsHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: runsBackend{
		trace: func(_ context.Context, actor domain.StudioActor, agentID string, runID int64) (domain.RunTrace, error) {
			if actor.TenantID != 7 || agentID != "writer-a" || runID != 31 {
				t.Fatalf("trace of %+v %s %d", actor, agentID, runID)
			}
			return domain.RunTrace{
				Run:  domain.AgentRun{ID: 31, Release: domain.AgentReleaseReference{AgentID: "writer-a", Version: "4"}, RequestID: "r-1", Status: domain.RunCompleted, CompletedAt: at},
				Turn: &domain.RunTraceTurn{ConversationID: "conv-1", TurnNo: 2, Status: domain.TurnCompleted, QueuedAt: at, StartedAt: at},
				Events: []domain.RunTraceEvent{
					{Sequence: 1, Kind: domain.RunTraceEventModelStep, At: at, Model: &domain.RunTraceModelStep{
						Journal: domain.RunTraceJournalRef{ExecutionStepID: 11, EntryNo: 1, Iteration: 1, Digest: strings.Repeat("a", 64)},
						Models:  []domain.ModelReference{{ProviderID: "anthropic", ModelID: "claude"}}, Usage: domain.Usage{InputTokens: 10, Currency: "USD"},
						Finish: domain.RunTraceFinishToolCalls, ToolCalls: 1}},
					{Sequence: 2, Kind: domain.RunTraceEventToolCall, At: at, Tool: &domain.RunTraceToolCall{Tool: domain.ToolReference{ToolID: "lookup", Version: "v1"},
						Outcome: domain.RunTraceToolError, ErrorClass: "rate_limited"}},
					{Sequence: 3, Kind: domain.RunTraceEventDecision, At: at, Decision: &domain.RunTraceDecisionEvent{Source: domain.RunTraceDecisionCharter,
						Action: "CAP-1", Charter: &domain.CharterAction{ActionID: domain.CharterRef{ID: "ACT-1"}, Disposition: "denied",
							Authority: []domain.CharterEvaluation{{Ref: &domain.CharterRef{ID: "G-1"}, Result: "denied"}}}}},
				},
				Truncated: true,
			}, nil
		},
	}}
	if feature := handler.Feature(); feature.ID != StudioFeatureRuns || feature.Version != 2 {
		t.Fatalf("feature = %+v", feature)
	}
	recorder := serveRuns(handler, api.StudioRunTracePath+"?agent_name=writer-a&run_id=31")
	var body api.RunTrace
	if recorder.Code != http.StatusOK || envelopeData(recorder, &body) != nil {
		t.Fatalf("GET = %d %s", recorder.Code, recorder.Body.String())
	}
	if body.Run.ID != 31 || body.Turn == nil || body.Turn.StartedAt == nil || body.Turn.CompletedAt != nil || !body.Truncated || len(body.Events) != 3 {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	step, call, decision := body.Events[0].ModelStep, body.Events[1].ToolCall, body.Events[2].Decision
	if step == nil || step.Finish != "tool_calls" || step.Usage == nil || step.Usage.InputTokens != 10 || step.Models[0].Model != "claude" || step.Journal.EntryNo != 1 {
		t.Fatalf("model step = %s", recorder.Body.String())
	}
	if call == nil || call.Outcome != "error" || call.ErrorClass != "rate_limited" || call.Usage != nil {
		t.Fatalf("tool call = %s", recorder.Body.String())
	}
	if decision == nil || decision.Source != "charter" || decision.Charter == nil || decision.Charter.Authority[0].Ref.ID != "G-1" {
		t.Fatalf("decision = %s", recorder.Body.String())
	}
}

func TestStudioRunTraceRouteRefusesABadRunAndHidesStoredRecordFailures(t *testing.T) {
	called := false
	backend := runsBackend{trace: func(_ context.Context, _ domain.StudioActor, _ string, runID int64) (domain.RunTrace, error) {
		called = true
		if runID == 404 {
			return domain.RunTrace{}, fmt.Errorf("%w: run 404", domain.ErrNotFound)
		}
		return domain.RunTrace{}, errors.New("run trace loop journal entry 3 of step 11: object digest mismatch: scout://sessions/scout/loop/7/r-1/11/3")
	}}
	handler := &StudioRunsHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
	for _, target := range []string{"?agent_name=writer-a", "?agent_name=writer-a&run_id=0", "?agent_name=writer-a&run_id=x"} {
		if recorder := serveRuns(handler, api.StudioRunTracePath+target); recorder.Code != http.StatusBadRequest || called {
			t.Fatalf("%s = %d (backend called %v)", target, recorder.Code, called)
		}
	}
	if recorder := serveRuns(handler, api.StudioRunTracePath+"?agent_name=writer-a&run_id=404"); recorder.Code != http.StatusNotFound {
		t.Fatalf("an unknown run = %d", recorder.Code)
	}
	recorder := serveRuns(handler, api.StudioRunTracePath+"?agent_name=writer-a&run_id=31")
	if recorder.Code != http.StatusInternalServerError || strings.Contains(recorder.Body.String(), "digest") || strings.Contains(recorder.Body.String(), "scout://") {
		t.Fatalf("a stored-record failure must be a sanitized 500: %d %s", recorder.Code, recorder.Body.String())
	}
}
