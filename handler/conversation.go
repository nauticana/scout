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

// PrincipalResolver maps an authenticated product session and selected agent
// onto the acting principal and the human whose authority it exercises. AgentID
// is empty for stream and cancel, where only the resolved tenant is used.
type PrincipalResolver interface {
	Resolve(ctx context.Context, session *keelmodel.UserSession, agentID string) (domain.Principal, domain.PrincipalRef, error)
}

// ConversationHandler is the HTTP-only bridge over a composed data plane.
type ConversationHandler struct {
	keelhandler.AbstractHandler
	Plane      contract.DataPlane
	Principals PrincipalResolver
	// BasePath defaults to /api/conversation/. Products can mount the same
	// routes under a product surface such as /api/wingmate/.
	BasePath string
}

// Routes returns the authenticated submit, replay stream, and cancel routes.
func (h *ConversationHandler) Routes() map[string]http.HandlerFunc {
	base := h.BasePath
	if strings.TrimSpace(base) == "" {
		base = api.ConversationDefaultBasePath
	}
	base = "/" + strings.Trim(strings.TrimSpace(base), "/") + "/"
	return map[string]http.HandlerFunc{
		base + api.ConversationTurnPath:   h.submit,
		base + api.ConversationStreamPath: h.stream,
		base + api.ConversationCancelPath: h.cancel,
	}
}

func (h *ConversationHandler) validate() error {
	if h.Plane == nil || h.Principals == nil {
		return fmt.Errorf("%w: conversation handler needs a data plane and principal resolver", domain.ErrNotReady)
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
		strings.TrimSpace(request.AgentID) == "" || len(request.Input) == 0 {
		h.writeError(w, r, fmt.Errorf("%w: request_id, conversation_id, agent_id, and input are required", domain.ErrValidation))
		return
	}
	principal, onBehalfOf, err := h.Principals.Resolve(r.Context(), session, request.AgentID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	turn := domain.TurnRequest{
		TenantContext: domain.TenantContext{TenantID: principal.TenantID, ScopeID: principal.ScopeID},
		Principal:     principal, OnBehalfOf: onBehalfOf,
		RequestID: request.RequestID, ConversationID: request.ConversationID,
		AgentID: request.AgentID, Input: append([]byte(nil), request.Input...),
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
	principal, _, err := h.Principals.Resolve(r.Context(), session, "")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	replies := h.Plane.Replies()
	if replies == nil {
		h.writeError(w, r, fmt.Errorf("%w: conversation replies are unavailable", domain.ErrNotReady))
		return
	}
	subscription, err := replies.SubscribeFrom(r.Context(), principal.TenantID, requestID, cursor)
	if errors.Is(err, domain.ErrReplayExpired) {
		h.writeStoredReply(w, r, principal.TenantID, requestID, cursor)
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
			reply, receiveErr = h.storedReply(r.Context(), principal.TenantID, requestID, cursor)
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
	principal, _, err := h.Principals.Resolve(r.Context(), session, "")
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
	if err = canceller.Cancel(r.Context(), principal.TenantID, request.RequestID, reason); err != nil {
		h.writeError(w, r, err)
		return
	}
	common.WriteJSON(w, http.StatusAccepted, api.ConversationTurnAccepted{RequestID: request.RequestID})
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
	payload, err := json.Marshal(conversationReply(reply))
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
	payload, marshalErr := json.Marshal(api.ConversationStreamError{Status: status, Detail: detail, RequestID: requestID})
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
	if len(reply.Payload) > 0 {
		var payload any
		if json.Unmarshal(reply.Payload, &payload) != nil {
			payload = string(reply.Payload)
		}
		frame["payload"] = payload
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
	var apiErr *keelhandler.APIError
	if errors.As(mapped, &apiErr) {
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
