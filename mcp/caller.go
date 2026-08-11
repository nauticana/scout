package mcp

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/mark3labs/mcp-go/server"
	"github.com/nauticana/keel/common"
	keelhandler "github.com/nauticana/keel/handler"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

type transportKey struct{}

// TransportFromContext reports the transport that admitted the call. An
// unstamped context is treated as remote so authentication fails closed.
func TransportFromContext(ctx context.Context) domain.MCPTransport {
	transport, _ := ctx.Value(transportKey{}).(domain.MCPTransport)
	return transport
}

func withTransport(ctx context.Context, transport domain.MCPTransport) context.Context {
	return context.WithValue(ctx, transportKey{}, transport)
}

// CallerResolver derives caller identity from server state. Tenant, actor,
// credential, scopes, and trust are never accepted as tool arguments.
type CallerResolver interface {
	Resolve(ctx context.Context) (domain.MCPCaller, error)
}

// HostCaller is the locally trusted identity used to enumerate the full
// catalog at composition time, before any client has connected.
func HostCaller() domain.MCPCaller {
	return domain.MCPCaller{Transport: domain.MCPTransportStdio, Authenticated: true, HostTrusted: true}
}

// Authorize enforces a tool's declared scopes. A host-trusted stdio caller is
// exempt because the local user already owns the process.
func Authorize(policy domain.MCPToolPolicy, caller domain.MCPCaller) error {
	if caller.HostTrusted {
		return nil
	}
	for _, scope := range policy.RequiredScopes {
		if !slices.Contains(caller.Scopes, scope) {
			return fmt.Errorf("%w: scope %q is required", domain.ErrForbidden, scope)
		}
	}
	return nil
}

// BaseCallerResolver reads keel authentication context. Remote transports
// without an authenticated identity are refused.
type BaseCallerResolver struct {
	// TrustHost admits an unauthenticated stdio caller, and is valid only for
	// a locally executed composition the user already owns.
	TrustHost bool
	// ActorID resolves the acting user when the product tracks one.
	ActorID func(context.Context) int64
}

func (resolver BaseCallerResolver) Resolve(ctx context.Context) (domain.MCPCaller, error) {
	transport := TransportFromContext(ctx)
	caller := domain.MCPCaller{
		TenantID:     identifier(ctx, common.PartnerID),
		CredentialID: identifier(ctx, common.ApiKeyID),
		Subject:      common.AsString(ctx.Value(common.Subject)),
		Scopes:       scopesFromContext(ctx),
		SessionID:    sessionFromContext(ctx),
		ClientIP:     keelhandler.ClientIPFromContext(ctx),
		Transport:    transport,
	}
	if resolver.ActorID != nil {
		caller.ActorID = resolver.ActorID(ctx)
	}
	caller.Authenticated = caller.Subject != "" || caller.CredentialID != 0
	caller.HostTrusted = resolver.TrustHost && transport == domain.MCPTransportStdio
	if !caller.Authenticated && !caller.HostTrusted {
		return caller, fmt.Errorf("%w: mcp %q transport requires an authenticated caller", domain.ErrUnauthorized, transport)
	}
	return caller, nil
}

// identifier reads a keel identity key. An absent key is zero, never the -1
// sentinel common.AsInt64 returns for unknown values.
func identifier(ctx context.Context, key common.ContextKey) int64 {
	value, _ := common.AsInt64OK(ctx.Value(key))
	return value
}

func scopesFromContext(ctx context.Context) []string {
	if principal, ok := ctx.Value(common.AuthPrincipal).(*keelport.Principal); ok && principal != nil {
		return principal.Scopes
	}
	return strings.FieldsFunc(common.AsString(ctx.Value(common.Scopes)), func(r rune) bool {
		return r == ' ' || r == ','
	})
}

func sessionFromContext(ctx context.Context) string {
	if session := server.ClientSessionFromContext(ctx); session != nil {
		return session.SessionID()
	}
	return ""
}

var _ CallerResolver = BaseCallerResolver{}
