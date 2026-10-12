package handler

import (
	"bytes"
	"context"
	"encoding/json"
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

type conversationCallerFunc func(context.Context, *keelmodel.UserSession) (domain.TenantContext, domain.PrincipalRef, error)

func (function conversationCallerFunc) Resolve(ctx context.Context, session *keelmodel.UserSession) (domain.TenantContext, domain.PrincipalRef, error) {
	return function(ctx, session)
}

type conversationIngressFunc func(context.Context, domain.TurnRequest) (contract.TurnReplySubscription, error)

func (function conversationIngressFunc) OpenTurn(ctx context.Context, request domain.TurnRequest) (contract.TurnReplySubscription, error) {
	return function(ctx, request)
}

func (conversationIngressFunc) Selectable(_ context.Context, tenant domain.TenantContext, person domain.PrincipalRef, aliasID string) ([]domain.SelectableAgent, error) {
	if tenant.TenantID != 7 || person.ID != "42" {
		return nil, domain.ErrForbidden
	}
	return []domain.SelectableAgent{{AgentID: aliasID + "-a", DisplayName: "A", Default: true, Release: "3"}}, nil
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
		Callers: conversationCallerFunc(func(_ context.Context, session *keelmodel.UserSession) (domain.TenantContext, domain.PrincipalRef, error) {
			if session.PartnerId != 7 {
				return domain.TenantContext{}, domain.PrincipalRef{}, errors.New("wrong session")
			}
			return domain.TenantContext{TenantID: 7, ScopeID: "team-a"}, domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "42"}, nil
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
		"request_id":"request-1","conversation_id":"conversation-1","agent_type":"wingmate","agent_name":"wingmate-b","input":{"prompt":"hello"}
	}`)))

	if recorder.Code != http.StatusAccepted || !subscription.closed {
		t.Fatalf("status = %d, subscription closed = %v, body = %s", recorder.Code, subscription.closed, recorder.Body.String())
	}
	if admitted.TenantContext.TenantID != 7 || admitted.TenantContext.ScopeID != "team-a" ||
		admitted.AliasID != "wingmate" || admitted.AgentID != "wingmate-b" || admitted.Principal.ID != "" || admitted.OnBehalfOf.ID != "42" || string(admitted.Input) != `{"prompt":"hello"}` {
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
	for _, path := range []string{"/api/wingmate/turn", "/api/wingmate/turn/stream", "/api/wingmate/turn/cancel", "/api/wingmate/agents", "/api/wingmate/turn/feedback", "/api/wingmate/turns"} {
		if routes[path] == nil {
			t.Fatalf("missing route %s", path)
		}
	}
	if len(routes) != 6 || api.ConversationTurnPath != "turn" {
		t.Fatalf("routes = %v", routes)
	}
}

func TestConversationAgentsListsSelectableAgents(t *testing.T) {
	handler := newConversationHandler(&conversationPlane{ingress: conversationIngressFunc(nil)})
	recorder := httptest.NewRecorder()
	handler.Routes()["/api/wingmate/agents"](recorder, authenticatedRequest(http.MethodGet, "/api/wingmate/agents?agent_type=writer", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"agent_name":"writer-a"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestConversationRefusesASessionWithNoPerson(t *testing.T) {
	handler := newConversationHandler(&conversationPlane{ingress: conversationIngressFunc(nil)})
	handler.Callers = conversationCallerFunc(func(context.Context, *keelmodel.UserSession) (domain.TenantContext, domain.PrincipalRef, error) {
		return domain.TenantContext{TenantID: 7}, domain.PrincipalRef{}, nil
	})
	recorder := httptest.NewRecorder()
	handler.Routes()["/api/wingmate/agents"](recorder, authenticatedRequest(http.MethodGet, "/api/wingmate/agents?agent_type=writer", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d", recorder.Code)
	}
}

type turnHistoryFunc func(context.Context, domain.TenantContext, domain.PrincipalRef, domain.TurnHistoryFilter) (domain.TurnHistoryPage, error)

func (function turnHistoryFunc) Turns(ctx context.Context, tenant domain.TenantContext, person domain.PrincipalRef, filter domain.TurnHistoryFilter) (domain.TurnHistoryPage, error) {
	return function(ctx, tenant, person, filter)
}

func TestConversationTurnsListsThePersonsHistory(t *testing.T) {
	handler := newConversationHandler(&conversationPlane{})
	path := "/api/wingmate/turns"
	recorder := httptest.NewRecorder()
	handler.Routes()[path](recorder, authenticatedRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("uncomposed status = %d", recorder.Code)
	}

	queued := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var asked domain.TurnHistoryFilter
	handler.History = turnHistoryFunc(func(_ context.Context, tenant domain.TenantContext, person domain.PrincipalRef, filter domain.TurnHistoryFilter) (domain.TurnHistoryPage, error) {
		if tenant.TenantID != 7 || person.ID != "42" {
			return domain.TurnHistoryPage{}, domain.ErrForbidden
		}
		asked = filter
		return domain.TurnHistoryPage{NextBefore: "c", Turns: []domain.TurnSummary{
			{RequestID: "r-2", AgentID: "writer", AgentVersion: "3", Status: domain.TurnCompleted, QueuedAt: queued, CompletedAt: queued,
				Currency: "JPY", Result: json.RawMessage(`{"ok":true}`)},
			{RequestID: "r-1", Status: domain.TurnQueued, QueuedAt: queued},
		}}, nil
	})
	recorder = httptest.NewRecorder()
	handler.Routes()[path](recorder, authenticatedRequest(http.MethodGet, path+"?work_ref=case-9&before=b&limit=2", nil))
	if recorder.Code != http.StatusOK || asked != (domain.TurnHistoryFilter{WorkRef: "case-9", Before: "b", Limit: 2}) {
		t.Fatalf("status = %d, filter = %+v, body = %s", recorder.Code, asked, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"next_before":"c"`, `"agent_name":"writer"`, `"release_version":"3"`, `"cost_minor":0`, `"currency":"JPY"`,
		`"exponent":0`, `"result":{"ok":true}`, `"completed_at":"2026-10-01T12:00:00Z"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %s: %s", want, body)
		}
	}
	if strings.Count(body, `"exponent"`) != 1 || strings.Count(body, `"completed_at"`) != 1 {
		t.Fatalf("an unsettled or open turn carries no currency or completion: %s", body)
	}

	recorder = httptest.NewRecorder()
	handler.Routes()[path](recorder, authenticatedRequest(http.MethodGet, path+"?limit=x", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed limit status = %d", recorder.Code)
	}
}
