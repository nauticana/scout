package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// toolBackendFake publishes one open and one write-scoped tool, and hides the
// write-scoped tool from remote callers without the write scope.
type toolBackendFake struct {
	call   domain.MCPToolCall
	result domain.MCPToolResult
}

var backendTools = []domain.MCPToolDefinition{
	{Name: "search", Description: "search things"},
	{Name: "submit", Description: "submit things", Policy: domain.MCPToolPolicy{RequiredScopes: []string{"write"}}},
}

func (backend *toolBackendFake) ListTools(_ context.Context, caller domain.MCPCaller) ([]domain.MCPToolDefinition, error) {
	if caller.HostTrusted || slices.Contains(caller.Scopes, "write") {
		return backendTools, nil
	}
	return backendTools[:1], nil
}

func (backend *toolBackendFake) ExecuteTool(_ context.Context, call domain.MCPToolCall) (domain.MCPToolResult, error) {
	backend.call = call
	return backend.result, nil
}

var _ contract.MCPToolBackend = (*toolBackendFake)(nil)

func remoteContext(scopes string) context.Context {
	ctx := withTransport(context.Background(), domain.MCPTransportStreamableHTTP)
	ctx = context.WithValue(ctx, common.Subject, "agent-1")
	ctx = context.WithValue(ctx, common.PartnerID, int64(7))
	return context.WithValue(ctx, common.Scopes, scopes)
}

func TestBaseCallerResolverFailsClosed(t *testing.T) {
	if _, err := (BaseCallerResolver{}).Resolve(context.Background()); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("unstamped context error = %v, want unauthorized", err)
	}
	stdio := withTransport(context.Background(), domain.MCPTransportStdio)
	if _, err := (BaseCallerResolver{}).Resolve(stdio); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("untrusted stdio error = %v, want unauthorized", err)
	}
	caller, err := BaseCallerResolver{TrustHost: true}.Resolve(stdio)
	if err != nil || !caller.HostTrusted {
		t.Fatalf("trusted stdio caller = %+v, err = %v", caller, err)
	}
}

func TestBaseCallerResolverReadsKeelContext(t *testing.T) {
	caller, err := BaseCallerResolver{ActorID: func(context.Context) int64 { return 42 }}.Resolve(remoteContext("read, write"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if caller.TenantID != 7 || caller.ActorID != 42 || caller.Subject != "agent-1" || caller.HostTrusted {
		t.Fatalf("caller = %+v", caller)
	}
	if len(caller.Scopes) != 2 || caller.Scopes[1] != "write" {
		t.Fatalf("scopes = %v", caller.Scopes)
	}
}

func TestAuthorize(t *testing.T) {
	caller := domain.MCPCaller{Scopes: []string{"read"}}
	if err := Authorize(domain.MCPToolPolicy{RequiredScopes: []string{"read"}}, caller); err != nil {
		t.Fatalf("granted scope refused: %v", err)
	}
	err := Authorize(domain.MCPToolPolicy{RequiredScopes: []string{"read", "write"}}, caller)
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("missing scope error = %v, want forbidden", err)
	}
	if err = Authorize(domain.MCPToolPolicy{RequiredScopes: []string{"write"}}, HostCaller()); err != nil {
		t.Fatalf("host caller refused: %v", err)
	}
}

func registeredServer(t *testing.T) (*BaseServer, *toolBackendFake) {
	t.Helper()
	backend := &toolBackendFake{result: domain.MCPToolResult{
		Data:     map[string]int{"n": 1},
		Evidence: []domain.MCPResourceLink{{URI: "scout://evidence/1", Name: "evidence"}},
	}}
	server := NewServer(ServerConfig{Name: "test", Version: "1.0.0", Source: "test"})
	if err := server.RegisterToolBackend(context.Background(), backend); err != nil {
		t.Fatalf("register backend: %v", err)
	}
	return server, backend
}

func call(t *testing.T, server *BaseServer, ctx context.Context, method string, params any) string {
	t.Helper()
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	encoded, err := json.Marshal(server.MCPServer().HandleMessage(ctx, request))
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return string(encoded)
}

func TestRegisterToolBackendScopesDiscovery(t *testing.T) {
	server, _ := registeredServer(t)
	readOnly := call(t, server, remoteContext("read"), "tools/list", map[string]any{})
	if !strings.Contains(readOnly, `"search"`) || strings.Contains(readOnly, `"submit"`) {
		t.Fatalf("read-only listing = %s", readOnly)
	}
	unauthenticated := call(t, server, withTransport(context.Background(), domain.MCPTransportSSE), "tools/list", map[string]any{})
	if strings.Contains(unauthenticated, `"search"`) {
		t.Fatalf("unauthenticated listing = %s", unauthenticated)
	}
}

func TestRegisterToolBackendEnforcesPolicy(t *testing.T) {
	server, backend := registeredServer(t)
	params := map[string]any{"name": "submit", "arguments": map[string]any{"id": float64(3)}}

	refused := call(t, server, remoteContext("read"), "tools/call", params)
	if !strings.Contains(refused, "not found") {
		t.Fatalf("unscoped call = %s", refused)
	}
	if backend.call.Name != "" {
		t.Fatalf("backend reached without the required scope: %+v", backend.call)
	}

	allowed := call(t, server, remoteContext("read write"), "tools/call", params)
	if !strings.Contains(allowed, `\"source\": \"test\"`) || !strings.Contains(allowed, "scout://evidence/1") {
		t.Fatalf("scoped call = %s", allowed)
	}
	if backend.call.Name != "submit" || backend.call.Caller.TenantID != 7 || backend.call.Arguments["id"] != float64(3) {
		t.Fatalf("backend call = %+v", backend.call)
	}
}

// The handler re-checks scopes even though discovery filtering hides the tool.
func TestBackendToolAuthorizesBeforeExecuting(t *testing.T) {
	backend := &toolBackendFake{}
	tool := backendTool{governed: governed{callers: BaseCallerResolver{}}, definition: backendTools[1], executor: backend}
	result, err := tool.Handle(remoteContext("read"), mcpgo.CallToolRequest{})
	if err != nil || !result.IsError || backend.call.Name != "" {
		t.Fatalf("unscoped handle = %+v, err = %v, call = %+v", result, err, backend.call)
	}
}

// Servers that register protocol values directly have no caller-scoped
// catalog, so the discovery filter must leave their tools alone.
func TestDirectRegistrationStaysVisible(t *testing.T) {
	server := NewServer(ServerConfig{Name: "test", Version: "1.0.0", Source: "test"})
	server.Register(Tool(mcpgo.NewTool("ping"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("pong"), nil
	}))
	listed := call(t, server, context.Background(), "tools/list", map[string]any{})
	if !strings.Contains(listed, `"ping"`) {
		t.Fatalf("listing = %s", listed)
	}
}

type deniedCall struct {
	caller domain.MCPCaller
	tool   string
	err    error
}

func deniedServer(t *testing.T) (*BaseServer, *toolBackendFake, *[]deniedCall) {
	t.Helper()
	var denied []deniedCall
	backend := &toolBackendFake{result: domain.MCPToolResult{Data: map[string]int{"n": 1}}}
	server := NewServer(ServerConfig{Name: "test", Version: "1.0.0", OnDenied: func(_ context.Context, caller domain.MCPCaller, tool string, err error) {
		denied = append(denied, deniedCall{caller: caller, tool: tool, err: err})
	}})
	if err := server.RegisterToolBackend(context.Background(), backend); err != nil {
		t.Fatalf("register backend: %v", err)
	}
	return server, backend, &denied
}

func TestOnDeniedObservesRefusedCalls(t *testing.T) {
	server, backend, denied := deniedServer(t)
	submit := map[string]any{"name": "submit"}

	call(t, server, remoteContext("read"), "tools/call", submit)
	call(t, server, withTransport(context.Background(), domain.MCPTransportStreamableHTTP), "tools/call", submit)
	if backend.call.Name != "" || len(*denied) != 2 {
		t.Fatalf("denied = %+v, backend call = %+v", *denied, backend.call)
	}
	if got := (*denied)[0]; got.tool != "submit" || got.caller.TenantID != 7 || !errors.Is(got.err, domain.ErrForbidden) {
		t.Fatalf("catalog refusal = %+v", got)
	}
	if got := (*denied)[1]; !errors.Is(got.err, domain.ErrUnauthorized) {
		t.Fatalf("unauthenticated refusal = %+v", got)
	}

	*denied = nil
	call(t, server, remoteContext("read write"), "tools/call", submit)
	call(t, server, remoteContext("read"), "tools/call", map[string]any{"name": "search"})
	call(t, server, remoteContext("read"), "tools/call", map[string]any{"name": "unknown"})
	if len(*denied) != 0 {
		t.Fatalf("admitted or unknown calls reported as denied: %+v", *denied)
	}
}

// A tool in the caller's catalog whose scopes the caller lacks is reported with the scope refusal.
func TestOnDeniedReportsMissingScopes(t *testing.T) {
	var denied []error
	backend := &definedSchemaBackend{definition: domain.MCPToolDefinition{Name: "admin", Description: "admin", Policy: domain.MCPToolPolicy{RequiredScopes: []string{"admin"}}}}
	server := NewServer(ServerConfig{Name: "test", Version: "1.0.0", OnDenied: func(_ context.Context, _ domain.MCPCaller, _ string, err error) {
		denied = append(denied, err)
	}})
	if err := server.RegisterToolBackend(context.Background(), backend); err != nil {
		t.Fatalf("register backend: %v", err)
	}
	result := call(t, server, remoteContext("read"), "tools/call", map[string]any{"name": "admin"})
	if !strings.Contains(result, "not found") || len(denied) != 1 || !errors.Is(denied[0], domain.ErrForbidden) || !strings.Contains(denied[0].Error(), `scope "admin"`) {
		t.Fatalf("result = %s, denied = %v", result, denied)
	}
}

func TestBackendToolReportsItsOwnRefusal(t *testing.T) {
	var tools []string
	backend := &toolBackendFake{}
	tool := backendTool{governed: governed{callers: BaseCallerResolver{}}, definition: backendTools[1], executor: backend,
		onDenied: func(_ context.Context, _ domain.MCPCaller, name string, _ error) { tools = append(tools, name) }}
	if result, _ := tool.Handle(remoteContext("read"), mcpgo.CallToolRequest{}); !result.IsError || len(tools) != 1 || tools[0] != "submit" {
		t.Fatalf("result = %+v, reported = %v", result, tools)
	}
}

func TestBackendToolReportsUnresolvedCaller(t *testing.T) {
	var denied []error
	tool := backendTool{governed: governed{callers: BaseCallerResolver{}}, definition: backendTools[0], executor: &toolBackendFake{},
		onDenied: func(_ context.Context, _ domain.MCPCaller, _ string, err error) { denied = append(denied, err) }}
	if result, _ := tool.Handle(withTransport(context.Background(), domain.MCPTransportStreamableHTTP), mcpgo.CallToolRequest{}); !result.IsError || len(denied) != 1 || !errors.Is(denied[0], domain.ErrUnauthorized) {
		t.Fatalf("result = %+v, denied = %v", result, denied)
	}
}
