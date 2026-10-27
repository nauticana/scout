package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type turnControlBackend struct {
	filter   domain.LiveTurnFilter
	open     domain.OpenOutcomeFilter
	hold     domain.TurnHoldRequest
	release  domain.TurnReleaseRequest
	lookup   domain.TurnOutcomeRequest
	attest   domain.TurnAttestRequest
	takeover domain.TurnTakeoverRequest
	act      domain.TurnActRequest
	handback domain.TurnHandbackRequest
	finish   domain.TurnFinishRequest
	err      error
}

var settledCall = domain.CallOutcome{
	Key:  domain.CallOutcomeKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11, CallID: "c1"},
	Tool: domain.ToolReference{ToolID: "shop.write", Version: "2"}, Status: domain.CallCompleted, DispatchFence: 2,
	DispatchedAt: time.Unix(1500, 0).UTC(), DeadlineAt: time.Unix(1600, 0).UTC(), DeadlinePassed: true,
	Resolution: domain.ResolvedByAttestation, ResolvedFence: 3, Resolver: domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"},
	ResolvedAt: time.Unix(1700, 0).UTC(), Note: "order shows shipped", Evidence: domain.ObjectRef{URI: "scout://e/1", Digest: strings.Repeat("a", 64)},
}

func (backend *turnControlBackend) LookupOutcome(_ context.Context, _ domain.StudioActor, request domain.TurnOutcomeRequest) (domain.CallOutcome, error) {
	backend.lookup = request
	return settledCall, backend.err
}

func (backend *turnControlBackend) AttestOutcome(_ context.Context, _ domain.StudioActor, request domain.TurnAttestRequest) (domain.AttestationResult, error) {
	backend.attest = request
	return domain.AttestationResult{Outcome: settledCall, AwaitsSecond: true}, backend.err
}

var _ contract.StudioTurnControlBackend = (*turnControlBackend)(nil)

var heldTurn = domain.LiveTurn{RequestID: "request-1", ConversationID: "c", TurnNo: 4, Status: domain.TurnHeld, Fence: 3,
	HoldCode: domain.HoldCheckpoint, QueuedAt: time.Unix(1000, 0).UTC(), HeldAt: time.Unix(2000, 0).UTC()}

func (backend *turnControlBackend) LiveTurns(_ context.Context, _ domain.StudioActor, filter domain.LiveTurnFilter) (domain.LiveTurnPage, error) {
	backend.filter = filter
	return domain.LiveTurnPage{Turns: []domain.LiveTurn{heldTurn}, NextBefore: "1000:request-1"}, backend.err
}

func (backend *turnControlBackend) OpenOutcomes(_ context.Context, _ domain.StudioActor, filter domain.OpenOutcomeFilter) (domain.OpenOutcomePage, error) {
	backend.open = filter
	if backend.err != nil {
		return domain.OpenOutcomePage{}, backend.err
	}
	return domain.OpenOutcomePage{Outcomes: []domain.OpenOutcome{{
		AgentID: "writer-a", Key: domain.CallOutcomeKey{TenantID: 7, RequestID: "request-1", ExecutionStepID: 11, CallID: "c1"},
		ConversationID: "c", TurnNo: 4, Tool: domain.ToolReference{ToolID: "shop.write", Version: "2"}, Status: domain.CallUnknown,
		DispatchedAt: time.Unix(1500, 0).UTC(), DeadlineAt: time.Unix(1600, 0).UTC(), DeadlinePassed: true,
	}}, NextBefore: "1600000000000:11:cmVxdWVzdC0x:c1"}, nil
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
	handler := &StudioTurnControlHandler{UserService: conversationUsers{}, Backend: backend}
	if feature := handler.Feature(); feature.ID != StudioFeatureTurnControl || feature.Object != domain.OperationAuthorizationObject ||
		strings.Join(feature.Actions, ",") != "VIEW,OPERATE,PREEMPT,RECONCILE" {
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

	recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnHoldPath, `{"agent_name":"writer-a","request_id":"request-1","mode":"preempt","reason":"runaway"}`)
	if recorder.Code != http.StatusOK || backend.hold.Mode != domain.HoldModePreempt {
		t.Fatalf("preempt = %d %s, request %+v", recorder.Code, recorder.Body.String(), backend.hold)
	}

	recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnReleasePath, `{"agent_name":"writer-a","request_id":"request-1","fence":3}`)
	var released api.LiveTurn
	if recorder.Code != http.StatusOK || envelopeData(recorder, &released) != nil || released.Status != "Q" || released.Fence != 4 || backend.release.Fence != 3 {
		t.Fatalf("release = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnLookupPath,
		`{"agent_name":"writer-a","request_id":"request-1","fence":3,"execution_step_id":11,"call_id":"c1"}`)
	var looked api.CallOutcome
	if recorder.Code != http.StatusOK || envelopeData(recorder, &looked) != nil ||
		backend.lookup != (domain.TurnOutcomeRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 3, ExecutionStepID: 11, CallID: "c1"}) ||
		looked.Status != "C" || looked.Resolution != "T" || looked.ResolverID != "9" || looked.Evidence == nil || looked.ResolvedAt == nil {
		t.Fatalf("lookup = %d %s, request %+v", recorder.Code, recorder.Body.String(), backend.lookup)
	}

	recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnAttestPath,
		`{"agent_name":"writer-a","request_id":"request-1","fence":3,"execution_step_id":11,"call_id":"c1","outcome":"completed","note":"order shows shipped","evidence":{"uri":"scout://e/1","digest":"`+strings.Repeat("a", 64)+`"}}`)
	var attested api.TurnAttestResult
	if recorder.Code != http.StatusOK || envelopeData(recorder, &attested) != nil || !attested.AwaitsSecond ||
		backend.attest.Outcome != domain.CallCompleted || backend.attest.Note != "order shows shipped" || backend.attest.CallID != "c1" ||
		backend.attest.Evidence.URI != "scout://e/1" {
		t.Fatalf("attest = %d %s, request %+v", recorder.Code, recorder.Body.String(), backend.attest)
	}
	if recorder := serveTurnControl(handler, http.MethodPost, api.StudioTurnAttestPath,
		`{"agent_name":"writer-a","request_id":"request-1","fence":3,"execution_step_id":11,"call_id":"c1","outcome":"done","note":"x"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unknown outcome word = %d, want 400", recorder.Code)
	}

	backend.err = fmt.Errorf("%w: AGENT_OPERATION RECONCILE", domain.ErrForbidden)
	if recorder := serveTurnControl(handler, http.MethodPost, api.StudioTurnAttestPath,
		`{"agent_name":"writer-a","request_id":"request-1","fence":3,"execution_step_id":11,"call_id":"c1","outcome":"failed","note":"x"}`); recorder.Code != http.StatusForbidden {
		t.Fatalf("attest without RECONCILE = %d, want 403", recorder.Code)
	}
}

func TestStudioOpenOutcomesRouteMapsTheFilterAndThePage(t *testing.T) {
	backend := &turnControlBackend{}
	handler := &StudioTurnControlHandler{UserService: conversationUsers{}, Backend: backend}
	recorder := serveTurnControl(handler, http.MethodGet, api.StudioOpenOutcomesPath+"?agent_name=writer-a&before=9:1:cg:c0&limit=5", "")
	if recorder.Code != http.StatusOK || backend.open != (domain.OpenOutcomeFilter{AgentID: "writer-a", Before: "9:1:cg:c0", Limit: 5}) {
		t.Fatalf("GET open = %d %s, filter %+v", recorder.Code, recorder.Body.String(), backend.open)
	}
	body := recorder.Body.String()
	for _, field := range []string{`"agent_name":"writer-a"`, `"request_id":"request-1"`, `"conversation_id":"c"`, `"turn_no":"4"`,
		`"execution_step_id":"11"`, `"call_id":"c1"`, `"tool_id":"shop.write"`, `"tool_version":"2"`, `"status":"U"`,
		`"deadline_passed":true`, `"next_before":"1600000000000:11:cmVxdWVzdC0x:c1"`} {
		if !strings.Contains(body, field) {
			t.Fatalf("open outcomes lack %s: %s", field, body)
		}
	}
	var page api.OpenOutcomePage
	if envelopeData(recorder, &page) != nil || len(page.Outcomes) != 1 || page.Outcomes[0].TurnNo != 4 ||
		!page.Outcomes[0].DeadlineAt.Equal(time.Unix(1600, 0)) || page.NextBefore == "" {
		t.Fatalf("page = %s", body)
	}
	if recorder := serveTurnControl(handler, http.MethodGet, api.StudioOpenOutcomesPath+"?limit=x", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed limit = %d, want 400", recorder.Code)
	}
	for err, want := range map[error]int{
		fmt.Errorf("%w: before is not an open outcome cursor", domain.ErrValidation): http.StatusBadRequest,
		fmt.Errorf("%w: AGENT_OPERATION VIEW on every agent", domain.ErrForbidden):   http.StatusForbidden,
	} {
		backend.err = err
		if recorder := serveTurnControl(handler, http.MethodGet, api.StudioOpenOutcomesPath+"?before=nonsense", ""); recorder.Code != want {
			t.Fatalf("%v = %d, want %d", err, recorder.Code, want)
		}
	}
}

type pauseBackend struct{ request domain.AgentPauseRequest }

func (backend *pauseBackend) Pause(_ context.Context, _ domain.StudioActor, request domain.AgentPauseRequest) (domain.AgentPauseResult, error) {
	backend.request = request
	return domain.AgentPauseResult{DraftRevision: 5, Held: 2, HoldRequested: 1}, nil
}

func TestStudioPauseRouteSuspendsAndReportsHolds(t *testing.T) {
	backend := &pauseBackend{}
	handler := &StudioPauseHandler{UserService: conversationUsers{}, Backend: backend}
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
		handler := &StudioTurnControlHandler{UserService: conversationUsers{}, Backend: backend}
		recorder := serveTurnControl(handler, http.MethodPost, api.StudioTurnReleasePath, `{"agent_name":"writer-a","request_id":"request-1","fence":2}`)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("%v = %d, want 409", err, recorder.Code)
		}
	}
}
