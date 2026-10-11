package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	keelhandler "github.com/nauticana/keel/handler"
	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ApprovalHandler is the HTTP bridge to the approval desk: signed-in deciders list and
// decide their requests, and a decision link is redeemed without a session.
type ApprovalHandler struct {
	keelhandler.AbstractHandler
	Desk contract.ApprovalDesk
}

// Routes returns the approval routes.
func (h *ApprovalHandler) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		api.ApprovalPendingPath: h.JSON(http.MethodGet, h.pending),
		api.ApprovalResolvePath: bounded(h.JSON(http.MethodPost, h.resolve)),
		api.ApprovalLinkPath:    bounded(h.JSONPublic(http.MethodPost, h.redeem)),
	}
}

func (h *ApprovalHandler) pending(ctx context.Context, session *keelmodel.UserSession, _ json.RawMessage) (any, error) {
	actor, err := sessionActor(session)
	if err != nil {
		return nil, err
	}
	requests, err := h.Desk.Pending(ctx, actor.TenantID, actor.Principal, 0)
	if err != nil {
		return mapServiceError(nil, err)
	}
	items := make([]api.ApprovalRequest, 0, len(requests))
	for _, request := range requests {
		items = append(items, apiApproval(request))
	}
	return items, nil
}

func (h *ApprovalHandler) resolve(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	actor, err := sessionActor(session)
	if err != nil {
		return nil, err
	}
	var request api.ApprovalResolveRequest
	if err = decode(body, &request); err != nil {
		return nil, err
	}
	result, err := h.Desk.Resolve(ctx, domain.ApprovalVerdict{
		RequestKey: domain.ApprovalKey{TenantID: actor.TenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID},
		Status:     domain.ApprovalStatus(request.Verdict), Decider: actor.Principal,
		ProposedDigest: request.ProposedDigest, Reason: request.Reason, DecidedAt: time.Now().UTC(),
		RevisedArguments: revisedArguments(request.RevisedArguments),
	})
	if err != nil {
		return mapServiceError(nil, err)
	}
	return apiApproval(result), nil
}

// revisedArguments treats an explicit JSON null as absent, as for every other optional field.
func revisedArguments(raw json.RawMessage) json.RawMessage {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	return raw
}

func (h *ApprovalHandler) redeem(ctx context.Context, body json.RawMessage) (any, error) {
	var request api.ApprovalLinkRequest
	if err := decode(body, &request); err != nil {
		return nil, err
	}
	key := domain.ApprovalKey{TenantID: request.TenantID, RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID}
	result, err := h.Desk.RedeemLink(ctx, request.Token, key, domain.ApprovalStatus(request.Verdict), request.Reason)
	if err != nil {
		return mapServiceError(nil, err)
	}
	return apiApproval(result), nil
}

func apiApproval(request domain.ApprovalRequest) api.ApprovalRequest {
	item := api.ApprovalRequest{
		RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID, ConversationID: request.ConversationID,
		ScopeID: request.ScopeID, RuleID: request.RuleID, Action: request.Action, Resource: request.Resource,
		OutputClass: string(request.Class), RiskTier: string(request.RiskTier), Summary: request.Summary,
		ProposedDigest: request.ProposedDigest, Status: string(request.Status), CreatedAt: request.CreatedAt,
		RevisesStepID: request.RevisesStepID,
	}
	if !request.DeadlineAt.IsZero() {
		deadline := request.DeadlineAt
		item.DeadlineAt = &deadline
	}
	return item
}
