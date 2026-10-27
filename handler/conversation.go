package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/nauticana/keel/common"
	keelhandler "github.com/nauticana/keel/handler"
	keelmodel "github.com/nauticana/keel/model"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const defaultCancelReason = "requested by client"

// CallerResolver maps an authenticated product session onto its tenant context and the
// person a turn acts for. The data plane resolves and authorizes the serving agent.
type CallerResolver interface {
	Resolve(ctx context.Context, session *keelmodel.UserSession) (domain.TenantContext, domain.PrincipalRef, error)
}

// SessionCaller is the CallerResolver of a product whose session partner is the tenant
// and whose signed-in user is the person; the scope is the tenant root.
type SessionCaller struct{}

func (SessionCaller) Resolve(_ context.Context, session *keelmodel.UserSession) (domain.TenantContext, domain.PrincipalRef, error) {
	if session == nil || session.PartnerId <= 0 || session.Id <= 0 {
		return domain.TenantContext{}, domain.PrincipalRef{}, fmt.Errorf("%w: a partner session is required", domain.ErrForbidden)
	}
	return domain.TenantContext{TenantID: session.PartnerId}, domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: strconv.Itoa(session.Id)}, nil
}

var _ CallerResolver = SessionCaller{}

// ConversationHandler is the HTTP-only bridge over a composed data plane.
type ConversationHandler struct {
	keelhandler.AbstractHandler
	Plane   contract.DataPlane
	Callers CallerResolver
	// Feedback records turn ratings and History lists the person's turns; nil answers its route as unavailable.
	Feedback contract.TurnFeedbackRecorder
	History  contract.TurnHistory
	// BasePath defaults to /api/conversation/. Products can mount the same
	// routes under a product surface such as /api/wingmate/.
	BasePath string
}

// Routes returns the authenticated conversation routes.
func (h *ConversationHandler) Routes() map[string]http.HandlerFunc {
	base := h.BasePath
	if strings.TrimSpace(base) == "" {
		base = api.ConversationDefaultBasePath
	}
	base = "/" + strings.Trim(strings.TrimSpace(base), "/") + "/"
	return map[string]http.HandlerFunc{
		base + api.ConversationTurnPath:     h.submit,
		base + api.ConversationStreamPath:   h.stream,
		base + api.ConversationCancelPath:   h.cancel,
		base + api.ConversationAgentsPath:   h.agents,
		base + api.ConversationFeedbackPath: h.feedback,
		base + api.ConversationTurnsPath:    h.turns,
	}
}

func (h *ConversationHandler) validate() error {
	if h.Plane == nil || h.Callers == nil {
		return fmt.Errorf("%w: conversation handler needs a data plane and caller resolver", domain.ErrNotReady)
	}
	return nil
}

func (h *ConversationHandler) submit(w http.ResponseWriter, r *http.Request) {
	if !h.RequireMethod(w, r, http.MethodPost) {
		return
	}
	var request api.ConversationTurnRequest
	session, ok := h.ReadAuthRequest(w, r, &request)
	if !ok {
		return
	}
	if err := h.validate(); err != nil {
		h.writeError(w, r, err)
		return
	}
	if strings.TrimSpace(request.RequestID) == "" || strings.TrimSpace(request.ConversationID) == "" ||
		strings.TrimSpace(request.AgentType) == "" || len(request.Input) == 0 {
		h.writeError(w, r, fmt.Errorf("%w: request_id, conversation_id, agent_type, and input are required", domain.ErrValidation))
		return
	}
	tenant, person, err := h.caller(r.Context(), session)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	turn := domain.TurnRequest{
		TenantContext: tenant, OnBehalfOf: person,
		RequestID: request.RequestID, ConversationID: request.ConversationID,
		AliasID: request.AgentType, AgentID: request.AgentName,
		Release: request.Release, ReleaseReason: request.ReleaseReason, WorkRef: request.WorkRef,
		Input: append([]byte(nil), request.Input...),
	}
	ingress := h.Plane.Ingress()
	if ingress == nil {
		h.writeError(w, r, fmt.Errorf("%w: conversation ingress is unavailable", domain.ErrNotReady))
		return
	}
	subscription, err := ingress.OpenTurn(r.Context(), turn)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if subscription == nil {
		h.writeError(w, r, fmt.Errorf("%w: conversation ingress returned no reply subscription", domain.ErrNotReady))
		return
	}
	if err = subscription.Close(); err != nil {
		h.writeError(w, r, err)
		return
	}
	common.WriteJSON(w, http.StatusAccepted, api.ConversationTurnAccepted{RequestID: request.RequestID})
}

func (h *ConversationHandler) stream(w http.ResponseWriter, r *http.Request) {
	if !h.RequireMethod(w, r, http.MethodGet) {
		return
	}
	r = keelhandler.EnsureRequestID(r)
	session, ok := h.RequireSession(w, r)
	if !ok {
		return
	}
	if err := h.validate(); err != nil {
		h.writeError(w, r, err)
		return
	}
	requestID := strings.TrimSpace(r.URL.Query().Get("request_id"))
	cursor, err := parseCursor(r.URL.Query().Get("cursor"))
	if err != nil || requestID == "" {
		h.writeError(w, r, fmt.Errorf("%w: request_id and a non-negative cursor are required", domain.ErrValidation))
		return
	}
	tenant, _, err := h.Callers.Resolve(r.Context(), session)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	replies := h.Plane.Replies()
	if replies == nil {
		h.writeError(w, r, fmt.Errorf("%w: conversation replies are unavailable", domain.ErrNotReady))
		return
	}
	subscription, err := replies.SubscribeFrom(r.Context(), tenant.TenantID, requestID, cursor)
	if errors.Is(err, domain.ErrReplayExpired) {
		h.writeStoredReply(w, r, tenant.TenantID, requestID, cursor)
		return
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if subscription == nil {
		h.writeError(w, r, fmt.Errorf("%w: conversation replies returned no subscription", domain.ErrNotReady))
		return
	}
	defer subscription.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeError(w, r, fmt.Errorf("%w: response streaming is unavailable", domain.ErrNotReady))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		reply, receiveErr := subscription.Receive(r.Context())
		if errors.Is(receiveErr, io.EOF) || errors.Is(receiveErr, context.Canceled) {
			return
		}
		if errors.Is(receiveErr, domain.ErrReplayExpired) {
			reply, receiveErr = h.storedReply(r.Context(), tenant.TenantID, requestID, cursor)
		}
		if receiveErr != nil {
			if h.writeSSEError(w, r, receiveErr) == nil {
				flusher.Flush()
			}
			return
		}
		if err = writeSSE(w, reply); err != nil {
			return
		}
		flusher.Flush()
		cursor = reply.Sequence + 1
		if reply.Final {
			return
		}
	}
}

func (h *ConversationHandler) cancel(w http.ResponseWriter, r *http.Request) {
	if !h.RequireMethod(w, r, http.MethodPost) {
		return
	}
	var request api.ConversationCancelRequest
	session, ok := h.ReadAuthRequest(w, r, &request)
	if !ok {
		return
	}
	if err := h.validate(); err != nil {
		h.writeError(w, r, err)
		return
	}
	if strings.TrimSpace(request.RequestID) == "" {
		h.writeError(w, r, fmt.Errorf("%w: request_id is required", domain.ErrValidation))
		return
	}
	tenant, _, err := h.Callers.Resolve(r.Context(), session)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" {
		reason = defaultCancelReason
	}
	canceller := h.Plane.Canceller()
	if canceller == nil {
		h.writeError(w, r, fmt.Errorf("%w: conversation cancellation is unavailable", domain.ErrNotReady))
		return
	}
	if err = canceller.Cancel(r.Context(), tenant.TenantID, request.RequestID, reason); err != nil {
		h.writeError(w, r, err)
		return
	}
	common.WriteJSON(w, http.StatusAccepted, api.ConversationTurnAccepted{RequestID: request.RequestID})
}

// feedback records the signed-in person's rating of a completed turn.
func (h *ConversationHandler) feedback(w http.ResponseWriter, r *http.Request) {
	if !h.RequireMethod(w, r, http.MethodPost) {
		return
	}
	var request api.ConversationFeedbackRequest
	session, ok := h.ReadAuthRequest(w, r, &request)
	if !ok {
		return
	}
	if err := h.validate(); err != nil {
		h.writeError(w, r, err)
		return
	}
	if h.Feedback == nil {
		h.writeError(w, r, fmt.Errorf("%w: turn feedback is not composed", domain.ErrNotReady))
		return
	}
	tenant, person, err := h.caller(r.Context(), session)
	if err == nil {
		err = h.Feedback.RecordFeedback(r.Context(), tenant, person, request.RequestID, request.Rating, request.Reason)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	common.WriteJSON(w, http.StatusAccepted, api.ConversationTurnAccepted{RequestID: request.RequestID})
}

// turns pages the signed-in person's turns, ?work_ref=&before=&limit=, newest first.
func (h *ConversationHandler) turns(w http.ResponseWriter, r *http.Request) {
	if !h.RequireMethod(w, r, http.MethodGet) {
		return
	}
	session, ok := h.RequireSession(w, r)
	if !ok {
		return
	}
	if err := h.validate(); err != nil {
		h.writeError(w, r, err)
		return
	}
	if h.History == nil {
		h.writeError(w, r, fmt.Errorf("%w: turn history is not composed", domain.ErrNotReady))
		return
	}
	query := r.URL.Query()
	limit, err := optionalInt(query.Get("limit"), 0)
	if err != nil {
		h.writeError(w, r, fmt.Errorf("%w: limit must be an integer", domain.ErrValidation))
		return
	}
	tenant, person, err := h.caller(r.Context(), session)
	var page domain.TurnHistoryPage
	if err == nil {
		page, err = h.History.Turns(r.Context(), tenant, person, domain.TurnHistoryFilter{
			WorkRef: query.Get("work_ref"), Before: query.Get("before"), Limit: int(limit),
		})
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := api.ConversationTurnPage{Turns: make([]api.ConversationTurn, 0, len(page.Turns)), NextBefore: page.NextBefore}
	for _, turn := range page.Turns {
		items.Turns = append(items.Turns, conversationTurn(turn))
	}
	common.WriteJSON(w, http.StatusOK, items)
}

func conversationTurn(turn domain.TurnSummary) api.ConversationTurn {
	item := api.ConversationTurn{
		RequestID: turn.RequestID, ConversationID: turn.ConversationID, AgentName: turn.AgentID, ReleaseVersion: turn.AgentVersion,
		Status: string(turn.Status), WorkRef: turn.WorkRef, TaskKind: turn.TaskKind, InputSummary: turn.InputSummary,
		ErrorText: turn.ErrorText, Result: turn.Result, CreatedAt: turn.QueuedAt, CompletedAt: optionalTime(turn.CompletedAt),
	}
	if turn.Currency != "" {
		item.CostMinor, item.Currency, item.Exponent = &turn.CostMinorUnits, turn.Currency, &turn.Exponent
	}
	return item
}

// caller resolves the session to a person: a turn from HTTP always acts for someone, so
// its authorization never takes the in-process path of delegated work.
func (h *ConversationHandler) caller(ctx context.Context, session *keelmodel.UserSession) (domain.TenantContext, domain.PrincipalRef, error) {
	tenant, person, err := h.Callers.Resolve(ctx, session)
	if err == nil && (person.Kind == "" || person.ID == "") {
		err = fmt.Errorf("%w: the session resolves to no person", domain.ErrForbidden)
	}
	return tenant, person, err
}

// agents lists the agents of ?agent_type= the caller may address, the default first.
func (h *ConversationHandler) agents(w http.ResponseWriter, r *http.Request) {
	if !h.RequireMethod(w, r, http.MethodGet) {
		return
	}
	session, ok := h.RequireSession(w, r)
	if !ok {
		return
	}
	if err := h.validate(); err != nil {
		h.writeError(w, r, err)
		return
	}
	tenant, person, err := h.caller(r.Context(), session)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	ingress := h.Plane.Ingress()
	if ingress == nil {
		h.writeError(w, r, fmt.Errorf("%w: conversation ingress is unavailable", domain.ErrNotReady))
		return
	}
	selectable, err := ingress.Selectable(r.Context(), tenant, person, r.URL.Query().Get("agent_type"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	items := make([]api.ConversationAgent, 0, len(selectable))
	for _, agent := range selectable {
		items = append(items, api.ConversationAgent{AgentName: agent.AgentID, DisplayName: agent.DisplayName, Default: agent.Default, Release: agent.Release})
	}
	common.WriteJSON(w, http.StatusOK, items)
}

func parseCursor(raw string) (int64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cursor < 0 {
		return 0, domain.ErrValidation
	}
	return cursor, nil
}

func writeSSE(w io.Writer, reply domain.TurnReply) error {
	payload, err := common.MarshalJSON(conversationReply(reply))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: turn\ndata: %s\n\n", payload)
	return err
}

// writeSSEError reports a failure after the 200 is committed, with the status it
// would have had. As with keel's HTTP error writer, 5xx detail stays server-side.
func (h *ConversationHandler) writeSSEError(w io.Writer, r *http.Request, err error) error {
	status, detail := http.StatusInternalServerError, err.Error()
	var apiErr *keelhandler.APIError
	if _, mapped := mapConversationError(nil, err); errors.As(mapped, &apiErr) {
		status, detail = apiErr.Status, apiErr.Msg
	}
	requestID := common.RequestIDFromContext(r.Context())
	if status >= http.StatusInternalServerError {
		if h.Journal != nil {
			h.Journal.Error(fmt.Sprintf("request_id=%s status=%d %s %s %s: %s", requestID, status, r.Method, r.URL.Path, http.StatusText(status), detail))
		}
		detail = "internal server error — see request_id in your logs"
	}
	payload, marshalErr := common.MarshalJSON(api.ConversationStreamError{Status: int32(status), Detail: detail, RequestID: requestID})
	if marshalErr != nil {
		return marshalErr
	}
	_, err = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	return err
}

func conversationReply(reply domain.TurnReply) map[string]any {
	events := reply.Events
	if events == nil {
		events = []domain.TurnEvent{}
	}
	frame := map[string]any{
		"request_id": reply.RequestID, "conversation_id": reply.ConversationID,
		"sequence": reply.Sequence, "events": events, "final": reply.Final,
	}
	if json.Valid(reply.Payload) {
		frame["payload"] = json.RawMessage(reply.Payload)
	} else if len(reply.Payload) > 0 {
		frame["payload"] = string(reply.Payload)
	}
	if reply.ErrorCode != "" {
		frame["error_code"] = reply.ErrorCode
	}
	if reply.AgentVersion != "" {
		frame["agent_version"] = reply.AgentVersion
	}
	if !reply.EmittedAt.IsZero() {
		frame["emitted_at"] = reply.EmittedAt
	}
	return frame
}

// storedReply answers an expired cursor; a turn still running has no stored result, so the cursor stays expired.
func (h *ConversationHandler) storedReply(ctx context.Context, tenantID int64, requestID string, cursor int64) (domain.TurnReply, error) {
	reply, err := h.Plane.StoredReply(ctx, tenantID, requestID, cursor)
	if errors.Is(err, domain.ErrConflict) {
		return domain.TurnReply{}, fmt.Errorf("%w: %v", domain.ErrReplayExpired, err)
	}
	return reply, err
}

func (h *ConversationHandler) writeStoredReply(w http.ResponseWriter, r *http.Request, tenantID int64, requestID string, cursor int64) {
	reply, err := h.storedReply(r.Context(), tenantID, requestID, cursor)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = writeSSE(w, reply)
}

func (h *ConversationHandler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	_, mapped := mapConversationError(nil, err)
	var carrier keelhandler.HeaderCarrier
	if errors.As(mapped, &carrier) {
		for key, values := range carrier.ErrorHeaders() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
	}
	if apiErr, ok := errors.AsType[*keelhandler.APIError](mapped); ok {
		h.WriteRequestError(r, w, apiErr.Status, http.StatusText(apiErr.Status), apiErr.Msg)
		return
	}
	h.WriteRequestError(r, w, http.StatusInternalServerError, "Internal Server Error", err.Error())
}

func mapConversationError(result any, err error) (any, error) {
	if err == nil {
		return result, nil
	}
	switch {
	case errors.Is(err, domain.ErrValidation):
		return nil, keelhandler.NewAPIError(http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrUnauthorized), errors.Is(err, domain.ErrPrincipalUnknown):
		return nil, keelhandler.NewAPIError(http.StatusUnauthorized, err.Error())
	case errors.Is(err, domain.ErrForbidden):
		return nil, keelhandler.NewAPIError(http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		return nil, keelhandler.NewAPIError(http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrConflict), errors.Is(err, domain.ErrRevisionConflict):
		return nil, keelhandler.NewAPIError(http.StatusConflict, err.Error())
	case errors.Is(err, domain.ErrRateLimited), errors.Is(err, domain.ErrBudgetExceeded):
		return nil, withErrorHeaders(keelhandler.NewAPIError(http.StatusTooManyRequests, err.Error()), err)
	case errors.Is(err, domain.ErrNotReady), errors.Is(err, domain.ErrCircuitOpen):
		return nil, withErrorHeaders(keelhandler.NewAPIError(http.StatusServiceUnavailable, err.Error()), err)
	case errors.Is(err, domain.ErrReplayExpired):
		return nil, keelhandler.NewAPIError(http.StatusGone, err.Error())
	default:
		return result, err
	}
}
