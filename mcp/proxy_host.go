package mcp

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// proxyHeaders show that a reverse proxy forwarded the request.
var proxyHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip"}

// WarnUnrewrittenHost wraps an MCP handler that listens on loopback behind a
// reverse proxy on the same host. mcp-go refuses a loopback request whose Host
// is not localhost (DNS-rebinding protection), so every call fails with 403
// unless the proxy rewrites Host. The first such proxied request calls warn
// once with the fix; the request is still passed on and refused.
func WarnUnrewrittenHost(next http.Handler, warn func(message string)) http.Handler {
	if warn == nil {
		return next
	}
	var once sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && local != nil &&
			loopbackHost(local.String()) && !loopbackHost(r.Host) && proxied(r) {
			once.Do(func() {
				warn("mcp: a proxied request reached the loopback listener with Host " + r.Host +
					"; mcp-go refuses it (DNS-rebinding protection). Rewrite Host to localhost at the proxy, e.g. Caddy `header_up Host localhost`.")
			})
		}
		next.ServeHTTP(w, r)
	})
}

func proxied(r *http.Request) bool {
	for _, header := range proxyHeaders {
		if r.Header.Get(header) != "" {
			return true
		}
	}
	return false
}

// loopbackHost applies mcp-go's rule: localhost or a loopback IP, with or without a port.
func loopbackHost(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = strings.Trim(address, "[]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
