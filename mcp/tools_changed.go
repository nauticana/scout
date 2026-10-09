package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// ToolsChange names the callers whose tool list changed: every caller of the
// tenant, or one actor when ActorID is set. It is the cross-process message
// ToolsChangedHandler consumes.
type ToolsChange struct {
	TenantID int64 `json:"tenant_id"`
	ActorID  int64 `json:"actor_id,omitempty"`
}

func (change ToolsChange) validate() error {
	if change.TenantID <= 0 || change.ActorID < 0 {
		return fmt.Errorf("%w: a tools change names a tenant and optionally an actor", domain.ErrValidation)
	}
	return nil
}

func (change ToolsChange) matches(caller domain.MCPCaller) bool {
	return caller.TenantID == change.TenantID && (change.ActorID == 0 || caller.ActorID == change.ActorID)
}

// PublishToolsChange asks every server subscribed to topic to notify the
// matching callers. Each server instance needs its own subscription.
func PublishToolsChange(ctx context.Context, publisher keelport.MessagePublisher, topic string, change ToolsChange) error {
	if err := change.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(change)
	if err != nil {
		return fmt.Errorf("encode tools change: %w", err)
	}
	if err = publisher.Publish(ctx, topic, data, nil); err != nil {
		return fmt.Errorf("publish tools change: %w", err)
	}
	return nil
}

// ToolsChangedHandler notifies the live sessions a published ToolsChange
// names. Failures go to onError and the message is acknowledged; with a nil
// onError they are returned, so the subscriber redelivers.
func (s *BaseServer) ToolsChangedHandler(onError func(error)) keelport.MessageHandler {
	report := func(err error) error {
		if err == nil || onError == nil {
			return err
		}
		onError(err)
		return nil
	}
	return func(ctx context.Context, message *keelport.Message) error {
		var change ToolsChange
		err := json.Unmarshal(message.Data, &change)
		if err == nil {
			err = change.validate()
		}
		if err != nil {
			return report(fmt.Errorf("%w: tools change message %s: %w", domain.ErrValidation, message.ID, err))
		}
		_, err = s.NotifyToolsChanged(ctx, change.matches)
		return report(err)
	}
}

// NotifyToolsChanged notifies matching live sessions that their tool list changed.
func (s *BaseServer) NotifyToolsChanged(ctx context.Context, match func(domain.MCPCaller) bool) (int, error) {
	if match == nil {
		return 0, fmt.Errorf("%w: a tools change matcher is required", domain.ErrValidation)
	}
	sent := 0
	var errs []error
	for _, session := range s.listeners.matching(match) {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if filtered, ok := session.(server.SessionWithSubscriptionFilter); ok {
			if filter, active := filtered.SubscriptionFilter(); active && !filter.ToolsListChanged {
				continue
			}
		}
		sessionCtx := s.mcp.WithContext(ctx, session)
		if err := s.mcp.SendNotificationToClient(sessionCtx, mcpgo.MethodNotificationToolsListChanged, nil); err != nil {
			errs = append(errs, fmt.Errorf("notify session %q of a tools change: %w", session.SessionID(), err))
			continue
		}
		sent++
	}
	return sent, errors.Join(errs...)
}

// trackListeners follows the sessions a notification can reach: registered
// sessions until they unregister, and subscriptions/listen streams while open.
func (s *BaseServer) trackListeners(hooks *server.Hooks) {
	hooks.AddOnRegisterSession(func(ctx context.Context, session server.ClientSession) {
		if !trackable(session) {
			return
		}
		caller, _ := s.governed().caller(ctx)
		s.listeners.register(session, caller)
	})
	hooks.AddOnUnregisterSession(func(_ context.Context, session server.ClientSession) {
		if trackable(session) {
			s.listeners.unregister(session)
		}
	})
	hooks.AddBeforeAny(func(ctx context.Context, _ any, _ mcpgo.MCPMethod, _ any) {
		if session := server.ClientSessionFromContext(ctx); trackable(session) && s.listeners.tracked(session) {
			if caller, err := s.governed().caller(ctx); err == nil {
				s.listeners.update(session, caller)
			}
		}
	})
	hooks.AddBeforeSubscriptionsListen(func(ctx context.Context, _ any, _ *mcpgo.SubscriptionsListenRequest) {
		session := server.ClientSessionFromContext(ctx)
		if !trackable(session) {
			return
		}
		caller, _ := s.governed().caller(ctx)
		s.listeners.listen(session, caller)
		context.AfterFunc(ctx, func() { s.listeners.unlisten(session) })
	})
}

// trackable reports whether session can key the listener map.
func trackable(session server.ClientSession) bool {
	return session != nil && reflect.TypeOf(session).Comparable()
}

type listener struct {
	caller     domain.MCPCaller
	registered bool
	listens    int
}

// toolListeners holds the sessions a notification can reach and their callers.
type toolListeners struct {
	mu       sync.Mutex
	sessions map[server.ClientSession]*listener
}

func newToolListeners() *toolListeners {
	return &toolListeners{sessions: make(map[server.ClientSession]*listener)}
}

func (l *toolListeners) entry(session server.ClientSession) *listener {
	entry := l.sessions[session]
	if entry == nil {
		entry = &listener{}
		l.sessions[session] = entry
	}
	return entry
}

func (l *toolListeners) register(session server.ClientSession, caller domain.MCPCaller) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entry(session)
	entry.registered = true
	if known(caller) {
		entry.caller = caller
	}
}

func (l *toolListeners) unregister(session server.ClientSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry := l.sessions[session]; entry != nil {
		entry.registered = false
		l.drop(session, entry)
	}
}

func (l *toolListeners) listen(session server.ClientSession, caller domain.MCPCaller) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entry(session)
	entry.listens++
	if known(caller) {
		entry.caller = caller
	}
}

func (l *toolListeners) unlisten(session server.ClientSession) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry := l.sessions[session]; entry != nil {
		entry.listens--
		l.drop(session, entry)
	}
}

func (l *toolListeners) drop(session server.ClientSession, entry *listener) {
	if !entry.registered && entry.listens <= 0 {
		delete(l.sessions, session)
	}
}

func (l *toolListeners) tracked(session server.ClientSession) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessions[session] != nil
}

func (l *toolListeners) update(session server.ClientSession, caller domain.MCPCaller) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry := l.sessions[session]; entry != nil {
		entry.caller = caller
	}
}

// matching snapshots listeners before invoking product code.
func (l *toolListeners) matching(match func(domain.MCPCaller) bool) []server.ClientSession {
	l.mu.Lock()
	entries := make(map[server.ClientSession]domain.MCPCaller, len(l.sessions))
	for session, entry := range l.sessions {
		entries[session] = entry.caller
	}
	l.mu.Unlock()
	var sessions []server.ClientSession
	for session, caller := range entries {
		if known(caller) && match(caller) {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

func known(caller domain.MCPCaller) bool { return caller.Authenticated || caller.HostTrusted }
