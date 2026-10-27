package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

var operatorPerson = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}

var operatedTurn = domain.LiveTurn{RequestID: "request-1", ConversationID: "c", TurnNo: 4, Status: domain.TurnOperated, Fence: 4,
	HoldCode: domain.HoldCheckpoint, QueuedAt: time.Unix(1000, 0).UTC(), HeldAt: time.Unix(2000, 0).UTC(),
	Holder: operatorPerson, HoldUntil: time.Unix(2900, 0).UTC()}

var toolEntry = domain.OperatorEntry{
	TenantID: 7, RequestID: "request-1", Fence: 4, EntryNo: 2, Kind: domain.OperatorTool, Actor: operatorPerson,
	Position: domain.OperatorPosition{StepNo: 2, ExecutionStepID: 11, AfterEntryNo: 3},
	Body: domain.OperatorEntryBody{Tool: &domain.OperatorToolUse{CallID: "op:4:2", ToolID: "shop.write", Version: "2",
		Arguments: []byte(`{"order":"o-1"}`), Status: domain.CallCompleted, Output: []byte(`{"ok":true}`), ResourceURIs: []string{"object://evidence"}}},
	Usage: domain.Usage{ToolCalls: 1, SearchQueries: 2, CostMinorUnits: 125, Currency: "USD"}, Exponent: 2, CreatedAt: time.Unix(2500, 0).UTC(),
}

func (backend *turnControlBackend) TakeOver(_ context.Context, _ domain.StudioActor, request domain.TurnTakeoverRequest) (domain.LiveTurn, error) {
	backend.takeover = request
	return operatedTurn, backend.err
}

func (backend *turnControlBackend) Act(_ context.Context, _ domain.StudioActor, request domain.TurnActRequest) (domain.TurnActResult, error) {
	backend.act = request
	outcome := settledCall
	return domain.TurnActResult{Entry: toolEntry, Outcome: &outcome, Turn: operatedTurn}, backend.err
}

func (backend *turnControlBackend) HandBack(_ context.Context, _ domain.StudioActor, request domain.TurnHandbackRequest) (domain.LiveTurn, error) {
	backend.handback = request
	return domain.LiveTurn{RequestID: "request-1", Status: domain.TurnQueued, Fence: 5}, backend.err
}

func (backend *turnControlBackend) Finish(_ context.Context, _ domain.StudioActor, request domain.TurnFinishRequest) (domain.LiveTurn, error) {
	backend.finish = request
	return domain.LiveTurn{RequestID: "request-1", Status: domain.TurnCompleted, Fence: 4, Holder: operatorPerson}, backend.err
}

func TestStudioTurnOperationRoutesMapRequestsAndResults(t *testing.T) {
	backend := &turnControlBackend{}
	handler := &StudioTurnControlHandler{UserService: conversationUsers{}, Backend: backend}

	recorder := serveTurnControl(handler, http.MethodPost, api.StudioTurnTakeoverPath, `{"agent_name":"writer-a","request_id":"request-1","fence":3}`)
	var taken api.LiveTurn
	if recorder.Code != http.StatusOK || envelopeData(recorder, &taken) != nil || taken.Status != "O" || taken.HolderID != "9" || taken.HoldUntil == nil ||
		backend.takeover != (domain.TurnTakeoverRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 3}) {
		t.Fatalf("takeover = %d %s, request %+v", recorder.Code, recorder.Body.String(), backend.takeover)
	}

	recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnActPath,
		`{"agent_name":"writer-a","request_id":"request-1","fence":4,"kind":"tool","tool_id":"shop.write","tool_version":"2","arguments":{"order":"o-1"}}`)
	var acted api.TurnActResult
	if recorder.Code != http.StatusOK || envelopeData(recorder, &acted) != nil || backend.act.Kind != domain.OperatorTool ||
		backend.act.Tool != (domain.ToolReference{ToolID: "shop.write", Version: "2"}) || string(backend.act.Arguments) != `{"order":"o-1"}` {
		t.Fatalf("act = %d %s, request %+v", recorder.Code, recorder.Body.String(), backend.act)
	}
	if entry := acted.Entry; entry.Kind != "T" || entry.Tool == nil || entry.Tool.Status != "C" || entry.CostMinorUnits != 125 ||
		entry.SearchQueries != 2 || len(entry.Tool.ResourceURIs) != 1 || entry.Currency != "USD" || entry.Exponent != 2 ||
		entry.StepNo != 2 || entry.AfterEntryNo != 3 || acted.Outcome == nil || acted.Turn.Status != "O" {
		t.Fatalf("act result = %s", recorder.Body.String())
	}
	for kind, want := range map[string]domain.OperatorEntryKind{"note": domain.OperatorNote, "reply": domain.OperatorReply, "withdraw": domain.OperatorWithdraw} {
		recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnActPath, `{"agent_name":"writer-a","request_id":"request-1","fence":4,"kind":"`+kind+`","text":"t","call_id":"c2"}`)
		if recorder.Code != http.StatusOK || backend.act.Kind != want {
			t.Fatalf("act %s = %d, kind %q", kind, recorder.Code, backend.act.Kind)
		}
	}
	// A handback instruction is the handback's own field, never an act.
	if recorder := serveTurnControl(handler, http.MethodPost, api.StudioTurnActPath, `{"agent_name":"writer-a","request_id":"request-1","fence":4,"kind":"handback"}`); recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unknown act kind = %d, want 400", recorder.Code)
	}

	recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnHandbackPath, `{"agent_name":"writer-a","request_id":"request-1","fence":4,"instruction":"retry the refund"}`)
	var handed api.LiveTurn
	if recorder.Code != http.StatusOK || envelopeData(recorder, &handed) != nil || handed.Status != "Q" ||
		backend.handback != (domain.TurnHandbackRequest{AgentID: "writer-a", RequestID: "request-1", Fence: 4, Instruction: "retry the refund"}) {
		t.Fatalf("handback = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = serveTurnControl(handler, http.MethodPost, api.StudioTurnFinishPath, `{"agent_name":"writer-a","request_id":"request-1","fence":4,"reply":"refunded"}`)
	var finished api.LiveTurn
	if recorder.Code != http.StatusOK || envelopeData(recorder, &finished) != nil || finished.Status != "C" || finished.HolderID != "9" ||
		string(backend.finish.Reply) != "refunded" || backend.finish.Fence != 4 {
		t.Fatalf("finish = %d %s", recorder.Code, recorder.Body.String())
	}
}

// A lapsed lease and a stale fence are conflicts; a call awaiting approval is a conflict, an
// unsupported claim a bad request, and an evidence rule the composition cannot evaluate 503.
func TestStudioTurnOperationMapsRefusals(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{domain.ErrFenceLost, http.StatusConflict}, {domain.ErrConflict, http.StatusConflict},
		{domain.ErrApprovalPending, http.StatusConflict}, {domain.ErrUnsupportedClaim, http.StatusBadRequest},
		{domain.ErrDegraded, http.StatusServiceUnavailable}, {domain.ErrForbidden, http.StatusForbidden},
	} {
		backend := &turnControlBackend{err: fmt.Errorf("%w: turn request-1", tc.err)}
		handler := &StudioTurnControlHandler{UserService: conversationUsers{}, Backend: backend}
		turn := `"agent_name":"writer-a","request_id":"request-1","fence":4`
		for path, body := range map[string]string{
			api.StudioTurnTakeoverPath: `{` + turn + `}`, api.StudioTurnActPath: `{` + turn + `,"kind":"note","text":"x"}`,
			api.StudioTurnHandbackPath: `{` + turn + `}`, api.StudioTurnFinishPath: `{` + turn + `,"reply":"x"}`,
		} {
			if recorder := serveTurnControl(handler, http.MethodPost, path, body); recorder.Code != tc.code {
				t.Fatalf("%s on %s = %d %s, want %d", tc.err, strings.TrimPrefix(path, api.StudioBasePath), recorder.Code, recorder.Body.String(), tc.code)
			}
		}
	}
}
