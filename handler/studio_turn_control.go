package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// StudioFeatureTurnControl is the Studio feature StudioTurnControlHandler serves.
const StudioFeatureTurnControl = "turn_control"

// StudioTurnControlHandler serves an agent's live turns, holding, releasing, and operating them, and
// looking up or attesting their calls' unknown outcomes.
type StudioTurnControlHandler struct {
	keelhandler.AbstractHandler
	Backend contract.StudioTurnControlBackend
}

// Feature is the manifest entry a product adds to StudioHandler.Features when it mounts these routes.
func (h *StudioTurnControlHandler) Feature() api.StudioFeature {
	return api.StudioFeature{
		ID: StudioFeatureTurnControl, Version: 1, Object: domain.OperationAuthorizationObject,
		Actions: []string{string(domain.OperationView), string(domain.OperationOperate), string(domain.OperationPreempt), string(domain.OperationReconcile)},
	}
}

// Routes returns GET turns/live?agent_name=&before=&limit=, GET turn?agent_name=&request_id=,
// POST turn/hold, POST turn/release, POST turn/outcome/lookup, POST turn/outcome/attest, and
// POST turn/takeover, turn/act, turn/handback, and turn/finish.
func (h *StudioTurnControlHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.StudioLiveTurnsPath:    query(&h.AbstractHandler, h.liveTurns),
		api.StudioTurnPath:         query(&h.AbstractHandler, h.turn),
		api.StudioTurnHoldPath:     command(&h.AbstractHandler, h.hold),
		api.StudioTurnReleasePath:  command(&h.AbstractHandler, h.release),
		api.StudioTurnLookupPath:   command(&h.AbstractHandler, h.lookup),
		api.StudioTurnAttestPath:   command(&h.AbstractHandler, h.attest),
		api.StudioTurnTakeoverPath: command(&h.AbstractHandler, h.takeover),
		api.StudioTurnActPath:      command(&h.AbstractHandler, h.act),
		api.StudioTurnHandbackPath: command(&h.AbstractHandler, h.handback),
		api.StudioTurnFinishPath:   command(&h.AbstractHandler, h.finish),
	}
}

func (h *StudioTurnControlHandler) takeover(ctx context.Context, actor domain.StudioActor, request api.TurnTakeoverRequest) (any, error) {
	turn, err := h.Backend.TakeOver(ctx, actor, domain.TurnTakeoverRequest{AgentID: request.AgentName, RequestID: request.RequestID, Fence: request.Fence})
	if err != nil {
		return nil, operationError(err)
	}
	return apiLiveTurn(turn), nil
}

// operationError gives the refusals only an operator act meets their Studio status: a call that
// awaits a person's approval is 409, a reply whose claims the turn's evidence does not support
// 400, and a composition that cannot evaluate the evidence rule 503.
func operationError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrApprovalPending):
		return fmt.Errorf("%w: %w", domain.ErrConflict, err)
	case errors.Is(err, domain.ErrUnsupportedClaim):
		return fmt.Errorf("%w: %w", domain.ErrValidation, err)
	case errors.Is(err, domain.ErrDegraded):
		return fmt.Errorf("%w: %w", domain.ErrNotReady, err)
	}
	return err
}

// operatorActKinds maps an act's wire kind onto its turn_operator_entry_kind code.
var operatorActKinds = map[string]domain.OperatorEntryKind{
	api.OperatorActNote: domain.OperatorNote, api.OperatorActTool: domain.OperatorTool,
	api.OperatorActReply: domain.OperatorReply, api.OperatorActWithdraw: domain.OperatorWithdraw,
}

func (h *StudioTurnControlHandler) act(ctx context.Context, actor domain.StudioActor, request api.TurnActRequest) (any, error) {
	kind, known := operatorActKinds[request.Kind]
	if !known {
		return nil, badRequest("kind must be note, tool, reply, or withdraw")
	}
	result, err := h.Backend.Act(ctx, actor, domain.TurnActRequest{
		AgentID: request.AgentName, RequestID: request.RequestID, Fence: request.Fence, Kind: kind, Text: request.Text,
		Tool: domain.ToolReference{ToolID: request.ToolID, Version: request.ToolVersion}, Arguments: request.Arguments, CallID: request.CallID,
	})
	if err != nil {
		return nil, operationError(err)
	}
	response := api.TurnActResult{Entry: apiOperatorEntry(result.Entry), Turn: apiLiveTurn(result.Turn), Published: result.Published}
	if result.Outcome != nil {
		outcome := apiCallOutcome(*result.Outcome)
		response.Outcome = &outcome
	}
	return response, nil
}

func (h *StudioTurnControlHandler) handback(ctx context.Context, actor domain.StudioActor, request api.TurnHandbackRequest) (any, error) {
	turn, err := h.Backend.HandBack(ctx, actor, domain.TurnHandbackRequest{
		AgentID: request.AgentName, RequestID: request.RequestID, Fence: request.Fence, Instruction: request.Instruction,
	})
	if err != nil {
		return nil, operationError(err)
	}
	return apiLiveTurn(turn), nil
}

func (h *StudioTurnControlHandler) finish(ctx context.Context, actor domain.StudioActor, request api.TurnFinishRequest) (any, error) {
	turn, err := h.Backend.Finish(ctx, actor, domain.TurnFinishRequest{
		AgentID: request.AgentName, RequestID: request.RequestID, Fence: request.Fence, Reply: []byte(request.Reply),
	})
	if err != nil {
		return nil, operationError(err)
	}
	return apiLiveTurn(turn), nil
}

func apiOperatorEntry(entry domain.OperatorEntry) api.OperatorEntry {
	result := api.OperatorEntry{
		Fence: entry.Fence, EntryNo: entry.EntryNo, Kind: string(entry.Kind), ActorKind: string(entry.Actor.Kind), ActorID: entry.Actor.ID,
		StepNo: entry.Position.StepNo, ExecutionStepID: entry.Position.ExecutionStepID, AfterEntryNo: entry.Position.AfterEntryNo,
		Text: entry.Body.Text, CallID: entry.Body.CallID, InputTokens: entry.Usage.InputTokens, OutputTokens: entry.Usage.OutputTokens,
		ToolCalls: entry.Usage.ToolCalls, SearchQueries: entry.Usage.SearchQueries, CostMinorUnits: entry.Usage.CostMinorUnits,
		Currency: entry.Usage.Currency, Exponent: entry.Exponent,
		CreatedAt: entry.CreatedAt,
	}
	if frame := entry.Body.Frame; frame != nil {
		sequence := frame.Sequence
		result.Reply, result.ReplySequence = string(frame.Payload), &sequence
	}
	if use := entry.Body.Tool; use != nil {
		result.Tool = &api.OperatorToolUse{
			CallID: use.CallID, ToolID: use.ToolID, ToolVersion: use.Version, Arguments: use.Arguments,
			Status: string(use.Status), Output: use.Output, IsError: use.IsError, ResourceURIs: use.ResourceURIs,
		}
	}
	return result
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

func (h *StudioTurnControlHandler) lookup(ctx context.Context, actor domain.StudioActor, request api.TurnOutcomeRequest) (any, error) {
	outcome, err := h.Backend.LookupOutcome(ctx, actor, domainOutcomeRequest(request))
	if err != nil {
		return nil, err
	}
	return apiCallOutcome(outcome), nil
}

func (h *StudioTurnControlHandler) attest(ctx context.Context, actor domain.StudioActor, request api.TurnAttestRequest) (any, error) {
	outcome := map[string]domain.CallOutcomeStatus{api.CallOutcomeCompleted: domain.CallCompleted, api.CallOutcomeFailed: domain.CallFailed}[request.Outcome]
	if outcome == "" {
		return nil, badRequest("outcome must be completed or failed")
	}
	attest := domain.TurnAttestRequest{TurnOutcomeRequest: domainOutcomeRequest(request.TurnOutcomeRequest), Outcome: outcome, Note: request.Note}
	if request.Evidence != nil {
		attest.Evidence = domain.ObjectRef{URI: request.Evidence.URI, Digest: request.Evidence.Digest}
	}
	result, err := h.Backend.AttestOutcome(ctx, actor, attest)
	if err != nil {
		return nil, err
	}
	return api.TurnAttestResult{Outcome: apiCallOutcome(result.Outcome), AwaitsSecond: result.AwaitsSecond}, nil
}

func domainOutcomeRequest(request api.TurnOutcomeRequest) domain.TurnOutcomeRequest {
	return domain.TurnOutcomeRequest{AgentID: request.AgentName, RequestID: request.RequestID, Fence: request.Fence,
		ExecutionStepID: request.ExecutionStepID, CallID: request.CallID}
}

func apiLiveTurn(turn domain.LiveTurn) api.LiveTurn {
	result := api.LiveTurn{
		RequestID: turn.RequestID, ConversationID: turn.ConversationID, TurnNo: turn.TurnNo, Status: string(turn.Status),
		Fence: turn.Fence, HoldCode: string(turn.HoldCode), HoldRequested: turn.HoldRequested, HoldReason: turn.HoldReason,
		CancelRequested: turn.CancelRequested, QueuedAt: turn.QueuedAt, StartedAt: optionalTime(turn.StartedAt), HeldAt: optionalTime(turn.HeldAt),
		HolderKind: string(turn.Holder.Kind), HolderID: turn.Holder.ID, HoldUntil: optionalTime(turn.HoldUntil),
	}
	for _, outcome := range turn.Outcomes {
		result.Outcomes = append(result.Outcomes, apiCallOutcome(outcome))
	}
	for _, entry := range turn.Entries {
		result.Entries = append(result.Entries, apiOperatorEntry(entry))
	}
	return result
}

func apiCallOutcome(outcome domain.CallOutcome) api.CallOutcome {
	result := api.CallOutcome{
		ExecutionStepID: outcome.Key.ExecutionStepID, CallID: outcome.Key.CallID, ToolID: outcome.Tool.ToolID, ToolVersion: outcome.Tool.Version,
		Status: string(outcome.Status), DispatchFence: outcome.DispatchFence, DispatchedAt: outcome.DispatchedAt, DeadlineAt: outcome.DeadlineAt,
		DeadlinePassed: outcome.DeadlinePassed, Resolution: string(outcome.Resolution), ResolvedFence: outcome.ResolvedFence,
		ResolverKind: string(outcome.Resolver.Kind), ResolverID: outcome.Resolver.ID, ResolvedAt: optionalTime(outcome.ResolvedAt), Note: outcome.Note,
	}
	if outcome.Evidence.URI != "" {
		result.Evidence = &api.Evidence{URI: outcome.Evidence.URI, Digest: outcome.Evidence.Digest}
	}
	return result
}
