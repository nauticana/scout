package mcp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

func loopbackRequest(host string, proxied bool) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	r.Host = host
	if proxied {
		r.Header.Set("X-Forwarded-For", "203.0.113.7")
	}
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8090}
	return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
}

func TestWarnUnrewrittenHostWarnsOnceForAProxiedPublicHost(t *testing.T) {
	var warnings []string
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0"})
	handler := WarnUnrewrittenHost(srv.ServeStreamableHTTP(), func(message string) { warnings = append(warnings, message) })
	for _, r := range []*http.Request{
		loopbackRequest("localhost", true),
		loopbackRequest("mcp.example.com", false),
		loopbackRequest("mcp.example.com", true),
		loopbackRequest("mcp.example.com", true),
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if r.Host == "mcp.example.com" && w.Code != http.StatusForbidden {
			t.Fatalf("mcp-go answered %d for an unrewritten Host; the guard this warns about changed", w.Code)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "header_up Host localhost") {
		t.Fatalf("warnings = %q", warnings)
	}
}

func TestServeStreamableHTTPPassesTransportOptions(t *testing.T) {
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0"})
	w := httptest.NewRecorder()
	srv.ServeStreamableHTTP(server.WithDisableLocalhostProtection(true)).ServeHTTP(w, loopbackRequest("mcp.example.com", true))
	if w.Code == http.StatusForbidden {
		t.Fatal("an option passed to ServeStreamableHTTP was ignored")
	}
}

func TestServeStreamableHTTPKeepsScoutContextHook(t *testing.T) {
	called := false
	option := server.WithHTTPContextFunc(func(ctx context.Context, _ *http.Request) context.Context {
		called = true
		return ctx
	})
	scout := false
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", ClientIPHook: func(ctx context.Context, _ *http.Request) context.Context {
		scout = true
		return ctx
	}})
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	srv.ServeStreamableHTTP(option).ServeHTTP(httptest.NewRecorder(), r)
	if called || !scout {
		t.Fatalf("option hook ran %v, Scout hook ran %v; a transport option must not replace Scout's context hook", called, scout)
	}
}
