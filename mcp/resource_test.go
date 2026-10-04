package mcp

import (
	"context"
	"slices"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// resourceBackendFake publishes an open resource, a scoped resource, and a
// scoped template, and hides the template from callers without "admin".
type resourceBackendFake struct{ reads []string }

var backendResources = []domain.MCPResourceDefinition{
	{URI: "scout://open", Name: "open"},
	{URI: "scout://report", Name: "report", Policy: domain.MCPResourcePolicy{RequiredScopes: []string{"reports"}}},
	{URITemplate: "scout://account/{id}", Name: "account", Policy: domain.MCPResourcePolicy{RequiredScopes: []string{"admin"}}},
}

func (backend *resourceBackendFake) ListResources(_ context.Context, caller domain.MCPCaller) ([]domain.MCPResourceDefinition, error) {
	if caller.HostTrusted || slices.Contains(caller.Scopes, "admin") {
		return backendResources, nil
	}
	return backendResources[:2], nil
}

func (backend *resourceBackendFake) ReadResource(_ context.Context, request domain.MCPResourceRequest) ([]domain.MCPResourceContent, error) {
	backend.reads = append(backend.reads, request.URI)
	return []domain.MCPResourceContent{{URI: request.URI, MIMEType: "text/plain", Text: "body"}}, nil
}

var _ contract.MCPResourceBackend = (*resourceBackendFake)(nil)

func resourceServer(t *testing.T) (*BaseServer, *resourceBackendFake) {
	t.Helper()
	backend := &resourceBackendFake{}
	server := NewServer(ServerConfig{Name: "test", Version: "1.0.0", Source: "test"})
	if err := server.RegisterResourceBackend(context.Background(), backend); err != nil {
		t.Fatalf("register: %v", err)
	}
	return server, backend
}

func TestResourceListingIsCallerScoped(t *testing.T) {
	server, _ := resourceServer(t)
	plain := call(t, server, remoteContext("read"), "resources/list", map[string]any{})
	if !strings.Contains(plain, "scout://open") || strings.Contains(plain, "scout://report") {
		t.Fatalf("unscoped listing = %s", plain)
	}
	scoped := call(t, server, remoteContext("reports"), "resources/list", map[string]any{})
	if !strings.Contains(scoped, "scout://report") {
		t.Fatalf("scoped listing = %s", scoped)
	}
	if templates := call(t, server, remoteContext("reports"), "resources/templates/list", map[string]any{}); strings.Contains(templates, "account") {
		t.Fatalf("hidden template listed = %s", templates)
	}
	if templates := call(t, server, remoteContext("admin"), "resources/templates/list", map[string]any{}); !strings.Contains(templates, "scout://account/{id}") {
		t.Fatalf("admin templates = %s", templates)
	}
}

func TestResourceReadIsAuthorized(t *testing.T) {
	server, backend := resourceServer(t)
	read := func(scopes, uri string) string {
		return call(t, server, remoteContext(scopes), "resources/read", map[string]any{"uri": uri})
	}
	if refused := read("read", "scout://report"); !strings.Contains(refused, "forbidden") {
		t.Fatalf("unscoped read = %s", refused)
	}
	if refused := read("reports", "scout://account/7"); !strings.Contains(refused, "not found") {
		t.Fatalf("read outside the caller's catalog = %s", refused)
	}
	if len(backend.reads) != 0 {
		t.Fatalf("backend reached without authorization: %v", backend.reads)
	}
	if allowed := read("reports", "scout://report"); !strings.Contains(allowed, `"body"`) {
		t.Fatalf("scoped read = %s", allowed)
	}
	if allowed := read("admin", "scout://account/7"); !strings.Contains(allowed, `"body"`) {
		t.Fatalf("admin template read = %s", allowed)
	}
}

// Directly registered resources are not in the backend catalog and stay listed.
func TestDirectResourceStaysListedBesideBackend(t *testing.T) {
	server, _ := resourceServer(t)
	server.RegisterResource(Resource(mcpgo.NewResource("scout://direct", "direct"), func(context.Context) (any, error) { return 1, nil }))
	listed := call(t, server, remoteContext("read"), "resources/list", map[string]any{})
	if !strings.Contains(listed, "scout://direct") || strings.Contains(listed, "scout://report") {
		t.Fatalf("listing = %s", listed)
	}
	unauthenticated := call(t, server, withTransport(context.Background(), domain.MCPTransportSSE), "resources/list", map[string]any{})
	if strings.Contains(unauthenticated, "scout://open") {
		t.Fatalf("unauthenticated listing = %s", unauthenticated)
	}
}
