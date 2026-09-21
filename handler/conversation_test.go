package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nauticana/keel/config"
	keelhandler "github.com/nauticana/keel/handler"
	keellimiter "github.com/nauticana/keel/limiter"
	keelmodel "github.com/nauticana/keel/model"
	"github.com/nauticana/keel/user"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type conversationUsers struct{ user.UserService }

func (conversationUsers) ParseJWT(string) (*keelmodel.UserSession, error) {
	return &keelmodel.UserSession{Id: 42, PartnerId: 7}, nil
}

type conversationPrincipalResolverFunc func(context.Context, *keelmodel.UserSession, string) (domain.Principal, domain.PrincipalRef, error)

func (function conversationPrincipalResolverFunc) Resolve(ctx context.Context, session *keelmodel.UserSession, agentID string) (domain.Principal, domain.PrincipalRef, error) {
	return function(ctx, session, agentID)
}

type conversationIngressFunc func(context.Context, domain.TurnRequest) (contract.TurnReplySubscription, error)

func (function conversationIngressFunc) OpenTurn(ctx context.Context, request domain.TurnRequest) (contract.TurnReplySubscription, error) {
	return function(ctx, request)
}

type conversationRepliesFunc func(context.Context, int64, string, int64) (contract.TurnReplySubscription, error)

func (function conversationRepliesFunc) SubscribeFrom(ctx context.Context, tenantID int64, requestID string, cursor int64) (contract.TurnReplySubscription, error) {
	return function(ctx, tenantID, requestID, cursor)
}

type conversationCancellerFunc func(context.Context, int64, string, string) error

func (function conversationCancellerFunc) Cancel(ctx context.Context, tenantID int64, requestID, reason string) error {
	return function(ctx, tenantID, requestID, reason)
}

type conversationSubscription struct {
	frames []domain.TurnReply
	end    error
	closed bool
}

func (*conversationSubscription) Route() string { return "test:route" }
func (subscription *conversationSubscription) Receive(context.Context) (domain.TurnReply, error) {
	if len(subscription.frames) == 0 {
		if subscription.end != nil {
			return domain.TurnReply{}, subscription.end
		}
		return domain.TurnReply{}, io.EOF
	}
	reply := subscription.frames[0]
	subscription.frames = subscription.frames[1:]
	return reply, nil
}
func (subscription *conversationSubscription) Close() error { subscription.closed = true; return nil }

type conversationPlane struct {
	ingress      contract.ConversationIngress
	replies      contract.ReplayTurnReplySubscriber
	canceller    contract.TurnCanceller
	stored       domain.TurnReply
	storedErr    error
	storedCursor int64
}

func (*conversationPlane) Runtime() contract.ConversationRuntime       { return nil }
func (*conversationPlane) Scheduler() contract.FairTurnScheduler       { return nil }
func (plane *conversationPlane) Ingress() contract.ConversationIngress { return plane.ingress }
func (plane *conversationPlane) Replies() contract.ReplayTurnReplySubscriber {
	return plane.replies
}
func (plane *conversationPlane) Canceller() contract.TurnCanceller { return plane.canceller }
func (*conversationPlane) Close() error                            { return nil }
func (plane *conversationPlane) StoredReply(_ context.Context, _ int64, _ string, cursor int64) (domain.TurnReply, error) {
	plane.storedCursor = cursor
	return plane.stored, plane.storedErr
}

func newConversationHandler(plane contract.DataPlane) *ConversationHandler {
	return &ConversationHandler{
		AbstractHandler: keelhandler.AbstractHandler{UserService: conversationUsers{}},
		Plane:           plane,
		Principals: conversationPrincipalResolverFunc(func(_ context.Context, session *keelmodel.UserSession, agentID string) (domain.Principal, domain.PrincipalRef, error) {
			if session.PartnerId != 7 {
				return domain.Principal{}, domain.PrincipalRef{}, errors.New("wrong session")
			}
			return domain.Principal{Kind: domain.PrincipalAgent, ID: agentID, TenantID: 7, ScopeID: "team-a"},
				domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}, nil
		}),
		BasePath: "/api/wingmate/",
	}
}

func authenticatedRequest(method, target string, body []byte) *http.Request {
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func configureConversationRequestLimit(t *testing.T) {
	t.Helper()
	previous := config.Config()
	config.SetConfig(&config.KeelConfig{MaxRequestSize: 1 << 20})
	t.Cleanup(func() { config.SetConfig(previous) })
}

func TestConversationSubmitMapsSessionIdentityAndAdmitsTurn(t *testing.T) {
	configureConversationRequestLimit(t)
	subscription := &conversationSubscription{}
	var admitted domain.TurnRequest
	plane := &conversationPlane{ingress: conversationIngressFunc(func(_ context.Context, request domain.TurnRequest) (contract.TurnReplySubscription, error) {
		admitted = request
		return subscription, nil
	})}
	handler := newConversationHandler(plane)
	recorder := httptest.NewRecorder()
	handler.Routes()["/api/wingmate/turn"](recorder, authenticatedRequest(http.MethodPost, "/api/wingmate/turn", []byte(`{
		"request_id":"request-1","conversation_id":"conversation-1","agent_id":"wingmate","input":{"prompt":"hello"}
	}`)))

	if recorder.Code != http.StatusAccepted || !subscription.closed {
		t.Fatalf("status = %d, subscription closed = %v, body = %s", recorder.Code, subscription.closed, recorder.Body.String())
	}
	if admitted.TenantContext.TenantID != 7 || admitted.TenantContext.ScopeID != "team-a" ||
		admitted.Principal.ID != "wingmate" || admitted.OnBehalfOf.ID != "42" || string(admitted.Input) != `{"prompt":"hello"}` {
		t.Fatalf("admitted = %+v, input = %s", admitted, admitted.Input)
	}
}

func TestConversationStreamFallsBackToStoredFinalFrame(t *testing.T) {
	plane := &conversationPlane{
		replies: conversationRepliesFunc(func(context.Context, int64, string, int64) (contract.TurnReplySubscription, error) {
			return nil, domain.ErrReplayExpired
		}),
		stored: domain.TurnReply{RequestID: "request-1", Sequence: 8, Payload: []byte(`{"answer":"done"}`), Final: true},
	}
	handler := newConversationHandler(plane)
	recorder := httptest.NewRecorder()
	handler.Routes()["/api/wingmate/turn/stream"](recorder,
		authenticatedRequest(http.MethodGet, "/api/wingmate/turn/stream?request_id=request-1&cursor=5", nil))

	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/event-stream" || plane.storedCursor != 5 {
		t.Fatalf("status = %d, headers = %v, cursor = %d", recorder.Code, recorder.Header(), plane.storedCursor)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: turn") || !strings.Contains(body, `"payload":{"answer":"done"}`) || !strings.Contains(body, `"final":true`) {
		t.Fatalf("SSE body = %s", body)
	}
}

func TestConversationStreamReportsAFailureAfterTheStreamOpened(t *testing.T) {
	for name, test := range map[string]struct {
		end, storedErr error
		want           string
	}{
		"receive failure":         {end: domain.ErrNotReady, want: `"status":503`},
		"expired under a running": {end: domain.ErrReplayExpired, storedErr: domain.ErrConflict, want: `"status":410`},
	} {
		t.Run(name, func(t *testing.T) {
			subscription := &conversationSubscription{frames: []domain.TurnReply{{RequestID: "request-1", Sequence: 5}}, end: test.end}
			plane := &conversationPlane{
				replies: conversationRepliesFunc(func(context.Context, int64, string, int64) (contract.TurnReplySubscription, error) {
					return subscription, nil
				}),
				storedErr: test.storedErr,
			}
			recorder := httptest.NewRecorder()
			newConversationHandler(plane).Routes()["/api/wingmate/turn/stream"](recorder,
				authenticatedRequest(http.MethodGet, "/api/wingmate/turn/stream?request_id=request-1&cursor=5", nil))

			body := recorder.Body.String()
			if !strings.Contains(body, `"sequence":5`) || !strings.Contains(body, "event: error") || !strings.Contains(body, test.want) || !subscription.closed {
				t.Fatalf("SSE body = %s, closed = %v", body, subscription.closed)
			}
			if test.storedErr != nil && plane.storedCursor != 6 {
				t.Fatalf("stored cursor = %d, want 6", plane.storedCursor)
			}
		})
	}
}

func TestConversationStreamSanitizesInternalFailure(t *testing.T) {
	subscription := &conversationSubscription{end: fmt.Errorf("database password is secret")}
	plane := &conversationPlane{replies: conversationRepliesFunc(func(context.Context, int64, string, int64) (contract.TurnReplySubscription, error) {
		return subscription, nil
	})}
	recorder := httptest.NewRecorder()
	newConversationHandler(plane).Routes()["/api/wingmate/turn/stream"](recorder,
		authenticatedRequest(http.MethodGet, "/api/wingmate/turn/stream?request_id=request-1&cursor=0", nil))

	body := recorder.Body.String()
	if strings.Contains(body, "database password") || !strings.Contains(body, `"status":500`) ||
		!strings.Contains(body, "internal server error") || !strings.Contains(body, `"request_id":`) {
		t.Fatalf("SSE body = %s", body)
	}
}

func TestConversationCancelUsesTenantAndDefaultReason(t *testing.T) {
	configureConversationRequestLimit(t)
	var tenantID int64
	var requestID, reason string
	plane := &conversationPlane{canceller: conversationCancellerFunc(func(_ context.Context, tenant int64, request, why string) error {
		tenantID, requestID, reason = tenant, request, why
		return nil
	})}
	handler := newConversationHandler(plane)
	recorder := httptest.NewRecorder()
	handler.Routes()["/api/wingmate/turn/cancel"](recorder,
		authenticatedRequest(http.MethodPost, "/api/wingmate/turn/cancel", []byte(`{"request_id":"request-1"}`)))

	if recorder.Code != http.StatusAccepted || tenantID != 7 || requestID != "request-1" || reason != defaultCancelReason {
		t.Fatalf("status = %d, cancel = %d/%s/%s", recorder.Code, tenantID, requestID, reason)
	}
}

func TestMapConversationErrorPreservesRetryAfter(t *testing.T) {
	_, mapped := mapConversationError(nil, &keellimiter.LimitError{Err: domain.ErrRateLimited, Scope: "turn.partner", After: 3 * time.Second})
	var apiErr *keelhandler.APIError
	if !errors.As(mapped, &apiErr) || apiErr.Status != http.StatusTooManyRequests || apiErr.Header.Get("Retry-After") != "3" {
		t.Fatalf("mapped = %#v", mapped)
	}
}

func TestConversationRoutesUseConfiguredBasePath(t *testing.T) {
	routes := newConversationHandler(&conversationPlane{}).Routes()
	for _, path := range []string{"/api/wingmate/turn", "/api/wingmate/turn/stream", "/api/wingmate/turn/cancel"} {
		if routes[path] == nil {
			t.Fatalf("missing route %s", path)
		}
	}
	if len(routes) != 3 || api.ConversationTurnPath != "turn" {
		t.Fatalf("routes = %v", routes)
	}
}
