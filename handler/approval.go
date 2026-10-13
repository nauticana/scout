package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	keelhandler "github.com/nauticana/keel/handler"
	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ApprovalHandler serves approval requests, decision links, and an optional confirmation inbox.
type ApprovalHandler struct {
	keelhandler.AbstractHandler
	Desk   contract.ApprovalDesk
	Inbox  contract.MCPConfirmationInbox
	Labels contract.Labeler
}

// Routes returns the approval routes, and the confirmation routes when an Inbox is composed.
func (h *ApprovalHandler) Routes() map[string]http.HandlerFunc {
	routes := map[string]http.HandlerFunc{
		api.ApprovalPendingPath: h.JSON(http.MethodGet, h.pending),
		api.ApprovalResolvePath: bounded(&h.AbstractHandler, h.JSON(http.MethodPost, h.resolve)),
		api.ApprovalLinkPath:    bounded(&h.AbstractHandler, h.JSONPublic(http.MethodPost, h.redeem)),
	}
	if h.Inbox != nil {
		routes[api.ConfirmationsPath] = sessionQuery(&h.AbstractHandler, h.confirmations)
		routes[api.ConfirmationDecidePath] = bounded(&h.AbstractHandler, h.JSON(http.MethodPost, h.decideConfirmation))
		routes[api.ConfirmationWithdrawPath] = bounded(&h.AbstractHandler, h.JSON(http.MethodPost, h.withdrawConfirmation))
		routes[api.ConfirmationReconcilePath] = bounded(&h.AbstractHandler, h.JSON(http.MethodPost, h.reconcileConfirmation))
	}
	return routes
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

// confirmations lists the person's inbox, ?since=&limit=: open confirmations and those created
// since an RFC 3339 time, newest first.
func (h *ApprovalHandler) confirmations(ctx context.Context, session *keelmodel.UserSession, query url.Values) (any, error) {
	person, err := sessionPerson(session)
	if err != nil {
		return nil, err
	}
	var since time.Time
	if raw := query.Get("since"); raw != "" {
		if since, err = time.Parse(time.RFC3339, raw); err != nil {
			return nil, badRequest("since must be an RFC 3339 time")
		}
	}
	limit, err := optionalInt(query.Get("limit"), 0)
	if err != nil {
		return nil, badRequest("limit must be an integer")
	}
	entries, err := h.Inbox.List(ctx, person, since, int(limit))
	if err != nil {
		return mapServiceError(nil, err)
	}
	items := make([]api.Confirmation, 0, len(entries))
	for _, entry := range entries {
		item := apiConfirmation(entry.MCPConfirmation)
		item.CanDecide, item.CanWithdraw, item.CanReconcile = entry.CanDecide, entry.CanWithdraw, entry.CanReconcile
		items = append(items, item)
	}
	return h.labeled(ctx, items...)
}

func (h *ApprovalHandler) decideConfirmation(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	var request api.ConfirmationDecideRequest
	person, err := personRequest(session, body, &request)
	if err != nil {
		return nil, err
	}
	decided, err := h.Inbox.Decide(ctx, person, confirmationKey(request.ConfirmationKey), request.Approve, request.Note)
	if err != nil {
		return mapServiceError(nil, err)
	}
	return h.labeledOne(ctx, apiConfirmation(decided))
}

func (h *ApprovalHandler) withdrawConfirmation(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	var request api.ConfirmationKey
	person, err := personRequest(session, body, &request)
	if err != nil {
		return nil, err
	}
	if err = h.Inbox.Withdraw(ctx, person, confirmationKey(request)); err != nil {
		return mapServiceError(nil, err)
	}
	return request, nil
}

func (h *ApprovalHandler) reconcileConfirmation(ctx context.Context, session *keelmodel.UserSession, body json.RawMessage) (any, error) {
	var request api.ConfirmationReconcileRequest
	person, err := personRequest(session, body, &request)
	if err != nil {
		return nil, err
	}
	reconciled, err := h.Inbox.Reconcile(ctx, person, confirmationKey(request.ConfirmationKey),
		domain.MCPConfirmationStatus(request.Outcome), request.Note)
	if err != nil {
		return mapServiceError(nil, err)
	}
	return h.labeledOne(ctx, apiConfirmation(reconciled))
}

func (h *ApprovalHandler) labeled(ctx context.Context, items ...api.Confirmation) ([]api.Confirmation, error) {
	labels := newPageLabels()
	for index := range items {
		labels.confirmation(&items[index])
	}
	if err := labels.apply(ctx, h.Labels); err != nil {
		return nil, err
	}
	return items, nil
}

func (h *ApprovalHandler) labeledOne(ctx context.Context, item api.Confirmation) (any, error) {
	items, err := h.labeled(ctx, item)
	if err != nil {
		return nil, err
	}
	return items[0], nil
}

// sessionPerson is the signed-in person; the confirmation inbox spans tenants, so no partner is needed.
func sessionPerson(session *keelmodel.UserSession) (domain.PrincipalRef, error) {
	if session == nil || session.Id <= 0 {
		return domain.PrincipalRef{}, keelhandler.NewAPIError(http.StatusUnauthorized, "user session required")
	}
	return domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: strconv.Itoa(session.Id)}, nil
}

func personRequest(session *keelmodel.UserSession, body json.RawMessage, request any) (domain.PrincipalRef, error) {
	person, err := sessionPerson(session)
	if err == nil {
		err = decode(body, request)
	}
	return person, err
}

func confirmationKey(key api.ConfirmationKey) domain.MCPConfirmationKey {
	return domain.MCPConfirmationKey{TenantID: key.TenantID, ID: key.ID}
}

func apiConfirmation(stored domain.MCPConfirmation) api.Confirmation {
	return api.Confirmation{
		TenantID: stored.TenantID, ID: stored.ID, Maker: principalItem(stored.Maker), ClientRef: stored.ClientRef,
		Tool: stored.Tool, PayloadDigest: stored.PayloadDigest, Preview: stored.Preview, Channel: string(stored.Channel),
		Status: string(stored.Status), ApprovalRequired: stored.ApprovalRequired, Expired: stored.Expired,
		ExpiresAt: stored.ExpiresAt, CreatedAt: stored.CreatedAt, DecidedAt: optionalTime(stored.DecidedAt),
		Decider: optionalPrincipal(stored.Decider), DecisionNote: stored.DecisionNote, Error: stored.Error,
		CompletedAt: optionalTime(stored.CompletedAt), Reconciler: optionalPrincipal(stored.Reconciler), ReconcileNote: stored.ReconcileNote,
	}
}

func optionalPrincipal(ref domain.PrincipalRef) *api.PrincipalRef {
	if ref.ID == "" {
		return nil
	}
	item := principalItem(ref)
	return &item
}

func apiApproval(request domain.ApprovalRequest) api.ApprovalRequest {
	item := api.ApprovalRequest{
		RequestID: request.RequestID, ExecutionStepID: request.ExecutionStepID, ConversationID: request.ConversationID,
		ScopeID: request.ScopeID, RuleID: request.RuleID, Action: request.Action, Resource: request.Resource,
		OutputClass: string(request.Class), RiskTier: string(request.RiskTier), Summary: request.Summary,
		ProposedDigest: request.ProposedDigest, Status: string(request.Status), CreatedAt: request.CreatedAt,
		RevisesStepID: request.RevisesStepID, RuleSource: string(request.Source),
		RouteID: request.Route.RouteID, RouteStep: request.Route.Step,
	}
	if request.Route.RouteID != "" {
		item.ApproverReference = &api.ApproverReference{Kind: string(request.Route.Reference.Kind), Ref: request.Route.Reference.Ref}
	}
	if !request.DeadlineAt.IsZero() {
		deadline := request.DeadlineAt
		item.DeadlineAt = &deadline
	}
	return item
}
