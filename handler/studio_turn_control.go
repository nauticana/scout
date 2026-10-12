package handler

import (
	"context"
	"net/http"
	"net/url"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureTurnControl is the Studio feature StudioTurnControlHandler serves.
const StudioFeatureTurnControl = "turn_control"

// StudioTurnControlHandler serves an agent's live turns and holding and releasing them.
type StudioTurnControlHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioTurnControlBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioTurnControlHandler) Feature() api.StudioFeature {
	return api.StudioFeature{
		ID: StudioFeatureTurnControl, Version: 1, Object: domain.OperationAuthorizationObject,
		Actions: []string{string(domain.OperationView), string(domain.OperationOperate)},
	}
}

// Routes returns GET turns/live?agent_name=&before=&limit=, GET turn?agent_name=&request_id=,
// POST turn/hold, and POST turn/release.
func (h *StudioTurnControlHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioLiveTurnsPath:   query(&h.AbstractHandler, h.liveTurns),
		api.StudioTurnPath:        query(&h.AbstractHandler, h.turn),
		api.StudioTurnHoldPath:    command(&h.AbstractHandler, h.hold),
		api.StudioTurnReleasePath: command(&h.AbstractHandler, h.release),
	}
}

func (h *StudioTurnControlHandler) liveTurns(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	limit, err := optionalInt(query.Get("limit"), 0)
	if err != nil {
		return nil, badRequest("limit must be an integer")
	}
	page, err := h.Backend.LiveTurns(ctx, actor, domain.LiveTurnFilter{AgentID: query.Get("agent_name"), Before: query.Get("before"), Limit: int(limit)})
	if err != nil {
		return nil, err
	}
	result := api.LiveTurnPage{Turns: make([]api.LiveTurn, 0, len(page.Turns)), NextBefore: page.NextBefore}
	for _, turn := range page.Turns {
		result.Turns = append(result.Turns, apiLiveTurn(turn))
	}
	return result, nil
}

func (h *StudioTurnControlHandler) turn(ctx context.Context, actor domain.StudioActor, query url.Values) (any, error) {
	turn, err := h.Backend.Turn(ctx, actor, query.Get("agent_name"), query.Get("request_id"))
	if err != nil {
		return nil, err
	}
	return apiLiveTurn(turn), nil
}

func (h *StudioTurnControlHandler) hold(ctx context.Context, actor domain.StudioActor, request api.TurnHoldRequest) (any, error) {
	outcome, err := h.Backend.Hold(ctx, actor, domain.TurnHoldRequest{
		AgentID: request.AgentName, RequestID: request.RequestID, Mode: domain.HoldMode(request.Mode), Reason: request.Reason,
	})
	if err != nil {
		return nil, err
	}
	return api.TurnHoldResult{Held: outcome.Held(), Status: string(outcome.Status), Fence: outcome.Fence}, nil
}

func (h *StudioTurnControlHandler) release(ctx context.Context, actor domain.StudioActor, request api.TurnReleaseRequest) (any, error) {
	turn, err := h.Backend.Release(ctx, actor, domain.TurnReleaseRequest{AgentID: request.AgentName, RequestID: request.RequestID, Fence: request.Fence})
	if err != nil {
		return nil, err
	}
	return apiLiveTurn(turn), nil
}

func apiLiveTurn(turn domain.LiveTurn) api.LiveTurn {
	return api.LiveTurn{
		RequestID: turn.RequestID, ConversationID: turn.ConversationID, TurnNo: turn.TurnNo, Status: string(turn.Status),
		Fence: turn.Fence, HoldCode: string(turn.HoldCode), HoldRequested: turn.HoldRequested, HoldReason: turn.HoldReason,
		CancelRequested: turn.CancelRequested, QueuedAt: turn.QueuedAt, StartedAt: optionalTime(turn.StartedAt), HeldAt: optionalTime(turn.HeldAt),
	}
}
