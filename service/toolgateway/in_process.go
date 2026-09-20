package toolgateway

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// InProcessScheme is the endpoint scheme of a tool served by a registered handler
// in the calling process: "inprocess://<tool id>".
const InProcessScheme = "inprocess"

// InProcessEndpoint is the endpoint to register for an in-process tool.
func InProcessEndpoint(toolID string) string { return InProcessScheme + "://" + toolID }

// InProcessHandler serves one tool. The tenant and principal come from the call,
// which the gateway authenticated; never from the model-supplied arguments.
type InProcessHandler func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error)

// InProcessTransport is the ToolTransport for tools implemented in the same
// binary. Other endpoints go to Next; without one they are refused.
type InProcessTransport struct {
	Next contract.ToolTransport

	mu       sync.RWMutex
	handlers map[string]InProcessHandler
}

// Register binds one tool id to its handler, once.
func (transport *InProcessTransport) Register(toolID string, handler InProcessHandler) error {
	toolID = strings.TrimSpace(toolID)
	if toolID == "" || handler == nil {
		return fmt.Errorf("%w: tool id and handler are required", domain.ErrValidation)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if _, exists := transport.handlers[toolID]; exists {
		return fmt.Errorf("%w: in-process tool %q is already registered", domain.ErrConflict, toolID)
	}
	if transport.handlers == nil {
		transport.handlers = make(map[string]InProcessHandler)
	}
	transport.handlers[toolID] = handler
	return nil
}

// Serves reports whether a handler is registered for the tool.
func (transport *InProcessTransport) Serves(toolID string) bool {
	transport.mu.RLock()
	defer transport.mu.RUnlock()
	_, ok := transport.handlers[toolID]
	return ok
}

func (transport *InProcessTransport) Invoke(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, credential []byte, timeout time.Duration) (domain.ToolResult, error) {
	if definition.Endpoint != InProcessEndpoint(definition.ToolID) {
		// A registered id received the no-secret in-process credential. Never let
		// a mismatched registry endpoint forward that call to a network transport.
		if transport.Serves(definition.ToolID) {
			return domain.ToolResult{}, fmt.Errorf("%w: registered in-process tool %q has endpoint %q", domain.ErrConflict, definition.ToolID, definition.Endpoint)
		}
		if transport.Next == nil {
			return domain.ToolResult{}, fmt.Errorf("%w: no transport serves endpoint of tool %q", domain.ErrNotReady, definition.ToolID)
		}
		return transport.Next.Invoke(ctx, call, definition, credential, timeout)
	}
	transport.mu.RLock()
	handler := transport.handlers[definition.ToolID]
	transport.mu.RUnlock()
	if handler == nil {
		return domain.ToolResult{}, fmt.Errorf("%w: in-process tool %q has no handler in this process", domain.ErrNotReady, definition.ToolID)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return handler(ctx, call)
}

// InProcessCredentials answers in-process tools with no secret, exercising the
// principal's own authority, and sends every other tool to Next.
type InProcessCredentials struct {
	Transport *InProcessTransport
	Next      contract.ToolCredentialProvider
}

func (provider *InProcessCredentials) Credential(ctx context.Context, principal domain.Principal, toolID, action, purpose string) ([]byte, domain.AuthorityRef, error) {
	if provider.Transport != nil && provider.Transport.Serves(toolID) {
		return nil, domain.AuthorityRef{Subject: domain.PrincipalRef{Kind: principal.Kind, ID: principal.ID}}, nil
	}
	if provider.Next == nil {
		return nil, domain.AuthorityRef{}, fmt.Errorf("%w: no credential provider for tool %q", domain.ErrNotReady, toolID)
	}
	return provider.Next.Credential(ctx, principal, toolID, action, purpose)
}

var (
	_ contract.ToolTransport          = (*InProcessTransport)(nil)
	_ contract.ToolCredentialProvider = (*InProcessCredentials)(nil)
)
