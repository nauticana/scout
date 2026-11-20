package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

// An in-process tool runs through the whole governed chain with the real
// transport, egress policy, and credential provider, and sees the gateway's
// authenticated tenant rather than anything the arguments claim.
func TestInProcessToolRunsThroughTheGovernedGateway(t *testing.T) {
	transport := &InProcessTransport{}
	var servedTenant int64
	var servedTimeout time.Duration
	if err := transport.Register("search", func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		servedTenant = call.TenantContext.TenantID
		deadline, _ := ctx.Deadline()
		servedTimeout = time.Until(deadline).Round(time.Second)
		return domain.ToolResult{Output: []byte(`{"hits":1}`)}, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := transport.Register("search", func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, nil
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for a second handler, got %v", err)
	}
	var calls []string
	gateway := governedGateway(&calls, nil)
	gateway.Registry = &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: InProcessEndpoint("search"), Timeout: 3 * time.Second}, nil
	}}
	gateway.Transport = transport
	gateway.Egress = &TableEgressPolicy{}
	gateway.Credentials = &InProcessCredentials{Transport: transport}
	call := validToolCall()
	call.Arguments = []byte(`{"tenant_id":999}`)
	result, err := gateway.Invoke(context.Background(), call)
	if err != nil || string(result.Output) != `{"hits":1}` {
		t.Fatalf("Invoke = %s, %v", result.Output, err)
	}
	if servedTenant != 7 {
		t.Fatalf("handler saw tenant %d; it must come from the call, never the arguments", servedTenant)
	}
	if servedTimeout != 3*time.Second {
		t.Fatalf("handler deadline = %s; the tool's registered timeout must win over the gateway default", servedTimeout)
	}
}

func TestInProcessTransportRefusesEndpointsItDoesNotServe(t *testing.T) {
	transport := &InProcessTransport{}
	remote := domain.ToolDefinition{ToolID: "remote", Endpoint: "https://tools.example/remote"}
	if _, err := transport.Invoke(context.Background(), domain.ToolCall{}, remote, nil, time.Second); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("want ErrNotReady without a next transport, got %v", err)
	}
	unregistered := domain.ToolDefinition{ToolID: "ghost", Endpoint: InProcessEndpoint("ghost")}
	if _, err := transport.Invoke(context.Background(), domain.ToolCall{}, unregistered, nil, time.Second); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("want ErrNotReady for a missing handler, got %v", err)
	}
	if _, _, err := (&InProcessCredentials{Transport: transport}).Credential(context.Background(), domain.Principal{}, "remote", "invoke", ""); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("a remote tool must not get the no-secret credential, got %v", err)
	}
	if err := transport.Register("remote", func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		return domain.ToolResult{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	transport.Next = fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		t.Fatal("a registered in-process tool must never fall through to a remote transport")
		return domain.ToolResult{}, nil
	})
	if _, err := transport.Invoke(context.Background(), domain.ToolCall{}, remote, nil, time.Second); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("want ErrConflict for a registered id with a remote endpoint, got %v", err)
	}
}

type egressRuleFake struct {
	keelport.DatabaseRepository
	allowed  map[string]bool
	inserted *[][]any
}

func (db egressRuleFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return db
}

func (db egressRuleFake) Query(_ context.Context, name string, args ...any) (*keelmodel.QueryResult, error) {
	if name == qEgressRuleInsert {
		*db.inserted = append(*db.inserted, args)
		return &keelmodel.QueryResult{}, nil
	}
	if args[1] == "search" && args[2] == "v1" && db.allowed[args[3].(string)+"://"+args[4].(string)] && args[5].(int) == 443 {
		return &keelmodel.QueryResult{Rows: [][]any{{1}}}, nil
	}
	return &keelmodel.QueryResult{}, nil
}

func (egressRuleFake) GenID() int64 { return 0 }

func TestTableEgressPolicyAdmitsOnlyRuledDestinations(t *testing.T) {
	policy := &TableEgressPolicy{DB: egressRuleFake{allowed: map[string]bool{"https://api.example.com": true}}}
	ctx := context.Background()
	if err := policy.ValidateDestination(ctx, 7, domain.ToolReference{ToolID: "search", Version: "v1"}, "https://API.example.com/v1/search"); err != nil {
		t.Fatalf("a ruled destination must pass on its default port: %v", err)
	}
	for _, endpoint := range []string{"https://evil.example.com/x", "https://api.example.com:8443/x", "http://api.example.com/x", "ftp://api.example.com/x"} {
		if err := policy.ValidateDestination(ctx, 7, domain.ToolReference{ToolID: "search", Version: "v1"}, endpoint); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("%s: want ErrForbidden, got %v", endpoint, err)
		}
	}
	if err := policy.ValidateDestination(ctx, 7, domain.ToolReference{ToolID: "crawl", Version: "v1"}, "https://api.example.com/x"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("another tool's rule must not admit: %v", err)
	}
	if err := policy.ValidateDestination(ctx, 7, domain.ToolReference{ToolID: "search", Version: "v1"}, "not a url"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation, got %v", err)
	}
}

func TestTableEgressPolicyAllowsARegisteredToolsEndpoint(t *testing.T) {
	var inserted [][]any
	policy := &TableEgressPolicy{DB: egressRuleFake{inserted: &inserted}}
	ctx := context.Background()
	tool := domain.ToolReference{ToolID: "search", Version: "v1"}
	if err := policy.Allow(ctx, 7, tool, "https://API.example.com/v1/search"); err != nil {
		t.Fatal(err)
	}
	if err := policy.Allow(ctx, 7, tool, InProcessEndpoint("search")); err != nil {
		t.Fatal(err)
	}
	if len(inserted) != 1 || fmt.Sprint(inserted[0]) != "[7 search v1 https api.example.com 443]" {
		t.Fatalf("inserted = %v", inserted)
	}
	if err := policy.Allow(ctx, 7, domain.ToolReference{ToolID: "search"}, "https://api.example.com"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("want ErrValidation without a version, got %v", err)
	}
	if err := policy.Allow(ctx, 7, tool, "ftp://api.example.com/x"); !errors.Is(err, domain.ErrForbidden) || len(inserted) != 1 {
		t.Fatalf("want ErrForbidden for an unsupported protocol, got %v", err)
	}
}

func TestGatewayStopsAtTheToolsOwnAttemptCeiling(t *testing.T) {
	var calls []string
	attempts := 0
	gateway := governedGateway(&calls, func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		attempts++
		return domain.ToolResult{Retryable: true}, nil
	})
	gateway.Registry = &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: "https://example.invalid/tool", MaxAttempts: 2}, nil
	}}
	gateway.Retry = RetryPolicy{MaxAttempts: 5}
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err == nil || attempts != 2 {
		t.Fatalf("attempts = %d (err %v), want the tool's ceiling of 2", attempts, err)
	}
}
