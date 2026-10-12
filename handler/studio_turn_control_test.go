package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type turnControlBackend struct {
	filter  domain.LiveTurnFilter
	hold    domain.TurnHoldRequest
	release domain.TurnReleaseRequest
	err     error
}

var _ contract.StudioTurnControlBackend = (*turnControlBackend)(nil)

var heldTurn = domain.LiveTurn{RequestID: "request-1", ConversationID: "c", TurnNo: 4, Status: domain.TurnHeld, Fence: 3,
	HoldCode: domain.HoldCheckpoint, QueuedAt: time.Unix(1000, 0).UTC(), HeldAt: time.Unix(2000, 0).UTC()}

func (backend *turnControlBackend) LiveTurns(_ context.Context, _ domain.StudioActor, filter domain.LiveTurnFilter) (domain.LiveTurnPage, error) {
	backend.filter = filter
	return domain.LiveTurnPage{Turns: []domain.LiveTurn{heldTurn}, NextBefore: "1000:request-1"}, backend.err
}

func (backend *turnControlBackend) Turn(context.Context, domain.StudioActor, string, string) (domain.LiveTurn, error) {
	return heldTurn, backend.err
}

func (backend *turnControlBackend) Hold(_ context.Context, _ domain.StudioActor, request domain.TurnHoldRequest) (domain.TurnHoldOutcome, error) {
	backend.hold = request
	return domain.TurnHoldOutcome{Status: domain.TurnHeld, Fence: 3}, backend.err
}

func (backend *turnControlBackend) Release(_ context.Context, _ domain.StudioActor, request domain.TurnReleaseRequest) (domain.LiveTurn, error) {
	backend.release = request
	return domain.LiveTurn{RequestID: "request-1", Status: domain.TurnQueued, Fence: 4}, backend.err
}

func serveTurnControl(handler *StudioTurnControlHandler, method, target, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	path, _, _ := strings.Cut(target, "?")
	handler.Routes()[path](recorder, authenticatedRequest(method, target, []byte(body)))
	return recorder
}

func TestStudioTurnControlRoutesMapRequestsAndResults(t *testing.T) {
	backend := &turnControlBackend{}
	handler := &StudioTurnControlHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
	if feature := handler.Feature(); feature.ID != StudioFeatureTurnControl || feature.Object != domain.OperationAuthorizationObject ||
		strings.Join(feature.Actions, ",") != "VIEW,OPERATE" {
		t.Fatalf("feature = %+v", feature)
	}

	recorder := serveTurnControl(handler, http.MethodGet, api.StudioLiveTurnsPath+"?agent_name=writer-a&before=9:r&limit=5", "")
	var page api.LiveTurnPage
	if recorder.Code != http.StatusOK || envelopeData(recorder, &page) != nil || backend.filter != (domain.LiveTurnFilter{AgentID: "writer-a", Before: "9:r", Limit: 5}) {
		t.Fatalf("GET live = %d %s, filter %+v", recorder.Code, recorder.Body.String(), backend.filter)
	}
	if len(page.Turns) != 1 || page.Turns[0].Status != "H" || page.Turns[0].HoldCode != "C" || page.Turns[0].Fence != 3 ||
		page.Turns[0].StartedAt != nil || page.Turns[0].HeldAt == nil || page.NextBefore != "1000:request-1" {
		t.Fatalf("page = %s", recorder.Body.String())
	}
	if recorder := serveTurnControl(handler, http.MethodGet, api.StudioLiveTurnsPath+"?agent_name=writer-a&limit=x", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed limit = %d", recorder.Code)
	}

	recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnHoldPath, `{"agent_name":"writer-a","request_id":"request-1","mode":"checkpoint","reason":"inspect"}`)
	var held api.TurnHoldResult
	if recorder.Code != http.StatusOK || envelopeData(recorder, &held) != nil || !held.Held || held.Fence != 3 ||
		backend.hold != (domain.TurnHoldRequest{AgentID: "writer-a", RequestID: "request-1", Mode: domain.HoldModeCheckpoint, Reason: "inspect"}) {
		t.Fatalf("hold = %d %s, request %+v", recorder.Code, recorder.Body.String(), backend.hold)
	}

	recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnReleasePath, `{"agent_name":"writer-a","request_id":"request-1","fence":3}`)
	var released api.LiveTurn
	if recorder.Code != http.StatusOK || envelopeData(recorder, &released) != nil || released.Status != "Q" || released.Fence != 4 || backend.release.Fence != 3 {
		t.Fatalf("release = %d %s", recorder.Code, recorder.Body.String())
	}
}

type pauseBackend struct{ request domain.AgentPauseRequest }

func (backend *pauseBackend) Pause(_ context.Context, _ domain.StudioActor, request domain.AgentPauseRequest) (domain.AgentPauseResult, error) {
	backend.request = request
	return domain.AgentPauseResult{DraftRevision: 5, Held: 2, HoldRequested: 1}, nil
}

func TestStudioPauseRouteSuspendsAndReportsHolds(t *testing.T) {
	backend := &pauseBackend{}
	handler := &StudioPauseHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
	recorder := httptest.NewRecorder()
	handler.Routes()[api.StudioPausePath](recorder, authenticatedRequest(http.MethodPost, api.StudioPausePath,
		[]byte(`{"agent_name":"writer-a","policy":"suspend","reason":"incident","expected_agent_revision":4}`)))
	var result api.AgentPauseResult
	if recorder.Code != http.StatusOK || envelopeData(recorder, &result) != nil || backend.request.Policy != domain.PauseSuspend ||
		result.Held != 2 || result.HoldRequested != 1 || result.ExpectedAgentRevision != 5 {
		t.Fatalf("pause = %d %s, request %+v", recorder.Code, recorder.Body.String(), backend.request)
	}
}

// A stale fence and a lost fence are conflicts, 409.
func TestStudioTurnControlMapsAStaleFenceToConflict(t *testing.T) {
	for _, err := range []error{domain.ErrConflict, domain.ErrFenceLost, domain.ErrTurnHeld} {
		backend := &turnControlBackend{err: fmt.Errorf("%w: turn request-1", err)}
		handler := &StudioTurnControlHandler{AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}}, Backend: backend}
		recorder := serveTurnControl(handler, http.MethodPost, api.StudioTurnReleasePath, `{"agent_name":"writer-a","request_id":"request-1","fence":2}`)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%v = %d, want 409", err, recorder.Code)
		}
	}
}
