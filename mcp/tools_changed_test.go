package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type fakeSession struct {
	id            string
	notifications chan mcpgo.JSONRPCNotification
}

func newFakeSession(id string) *fakeSession {
	return &fakeSession{id: id, notifications: make(chan mcpgo.JSONRPCNotification, 4)}
}

func (s *fakeSession) Initialize()                                           {}
func (s *fakeSession) Initialized() bool                                     { return true }
func (s *fakeSession) NotificationChannel() chan<- mcpgo.JSONRPCNotification { return s.notifications }
func (s *fakeSession) SessionID() string                                     { return s.id }

func (s *fakeSession) received() []string {
	var methods []string
	for {
		select {
		case notification := <-s.notifications:
			methods = append(methods, notification.Method)
		default:
			return methods
		}
	}
}

// subscribedSession opted in to the given notification types through subscriptions/listen.
type subscribedSession struct {
	*fakeSession
	filter mcpgo.SubscriptionFilter
}

func (s *subscribedSession) SetSubscriptionFilter(filter mcpgo.SubscriptionFilter) { s.filter = filter }
func (s *subscribedSession) SubscriptionFilter() (mcpgo.SubscriptionFilter, bool) {
	return s.filter, true
}

func tenantContext(tenant int64) context.Context {
	return context.WithValue(remoteContext("read"), common.PartnerID, tenant)
}

func TestNotifyToolsChangedReachesMatchingLiveSessions(t *testing.T) {
	srv, _ := registeredServer(t)
	seven, eight := newFakeSession("seven"), newFakeSession("eight")
	unsubscribed := &subscribedSession{fakeSession: newFakeSession("unsubscribed")}
	for _, registered := range []struct {
		session server.ClientSession
		tenant  int64
	}{{seven, 7}, {eight, 8}, {unsubscribed, 7}} {
		if err := srv.MCPServer().RegisterSession(tenantContext(registered.tenant), registered.session); err != nil {
			t.Fatalf("register %s: %v", registered.session.SessionID(), err)
		}
	}
	change := ToolsChange{TenantID: 7}
	sent, err := srv.NotifyToolsChanged(t.Context(), change.matches)
	if err != nil || sent != 1 {
		t.Fatalf("sent = %d, err = %v", sent, err)
	}
	if got := seven.received(); len(got) != 1 || got[0] != string(mcpgo.MethodNotificationToolsListChanged) {
		t.Fatalf("tenant 7 received %v", got)
	}
	if got := append(eight.received(), unsubscribed.received()...); len(got) != 0 {
		t.Fatalf("unmatched or unsubscribed sessions received %v", got)
	}

	// A request on the session refreshes its caller; unregistering stops notifications.
	call(t, srv, srv.MCPServer().WithContext(tenantContext(8), seven), "tools/list", map[string]any{})
	if sent, _ = srv.NotifyToolsChanged(t.Context(), change.matches); sent != 0 {
		t.Fatalf("session moved to tenant 8 still notified for tenant 7: %d", sent)
	}
	srv.MCPServer().UnregisterSession(context.Background(), seven.SessionID())
	srv.MCPServer().UnregisterSession(context.Background(), eight.SessionID())
	if sent, _ = srv.NotifyToolsChanged(t.Context(), func(domain.MCPCaller) bool { return true }); sent != 0 {
		t.Fatalf("unregistered sessions notified: %d", sent)
	}
}

func TestNotifyToolsChangedValidatesAndHonorsContext(t *testing.T) {
	srv, _ := registeredServer(t)
	if _, err := srv.NotifyToolsChanged(t.Context(), nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil matcher error = %v", err)
	}
	session := newFakeSession("seven")
	srv.listeners.register(session, domain.MCPCaller{TenantID: 7, Authenticated: true})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if sent, err := srv.NotifyToolsChanged(ctx, func(domain.MCPCaller) bool { return true }); sent != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("sent = %d, err = %v", sent, err)
	}
}

func TestListenStreamsAreTrackedWhileOpen(t *testing.T) {
	listeners := newToolListeners()
	session := newFakeSession("stdio")
	caller := domain.MCPCaller{TenantID: 7, Authenticated: true}
	listeners.register(session, caller)
	listeners.listen(session, caller)
	listeners.unlisten(session)
	if len(listeners.matching(func(domain.MCPCaller) bool { return true })) != 1 {
		t.Fatal("a registered session was dropped when its listen stream closed")
	}
	ephemeral := newFakeSession("")
	listeners.listen(ephemeral, caller)
	listeners.unregister(session)
	if got := listeners.matching(func(domain.MCPCaller) bool { return true }); len(got) != 1 || got[0] != server.ClientSession(ephemeral) {
		t.Fatalf("matching = %v", got)
	}
	listeners.unlisten(ephemeral)
	if len(listeners.sessions) != 0 {
		t.Fatalf("closed listeners kept: %d", len(listeners.sessions))
	}
}

type capturePublisher struct {
	topic string
	data  []byte
}

func (p *capturePublisher) Publish(_ context.Context, topic string, data []byte, _ map[string]string) error {
	p.topic, p.data = topic, data
	return nil
}

func (p *capturePublisher) Close() error { return nil }

var _ keelport.MessagePublisher = (*capturePublisher)(nil)

func TestToolsChangeCrossesProcesses(t *testing.T) {
	publisher := &capturePublisher{}
	if err := PublishToolsChange(context.Background(), publisher, "mcp-tools", ToolsChange{TenantID: 0}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("tenantless change error = %v", err)
	}
	if err := PublishToolsChange(context.Background(), publisher, "mcp-tools", ToolsChange{TenantID: 7, ActorID: 3}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	srv, _ := registeredServer(t)
	actor3, actor4 := newFakeSession("3"), newFakeSession("4")
	for actor, session := range map[int64]*fakeSession{3: actor3, 4: actor4} {
		srv.listeners.register(session, domain.MCPCaller{TenantID: 7, ActorID: actor, Authenticated: true})
	}
	var reported []error
	handle := srv.ToolsChangedHandler(func(err error) { reported = append(reported, err) })
	if err := handle(context.Background(), &keelport.Message{ID: "1", Data: publisher.data}); err != nil || len(reported) != 0 {
		t.Fatalf("handle = %v, reported = %v", err, reported)
	}
	if len(actor3.received()) != 1 || len(actor4.received()) != 0 {
		t.Fatal("the change reached the wrong actor")
	}

	malformed := &keelport.Message{ID: "2", Data: json.RawMessage(`{"tenant_id":"seven"}`)}
	if err := handle(context.Background(), malformed); err != nil || len(reported) != 1 || !errors.Is(reported[0], domain.ErrValidation) {
		t.Fatalf("malformed handle = %v, reported = %v", err, reported)
	}
	if err := srv.ToolsChangedHandler(nil)(context.Background(), malformed); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("malformed without a sink = %v, want it returned", err)
	}
}
