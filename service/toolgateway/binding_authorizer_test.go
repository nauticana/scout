package toolgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func boundRegistry(bound ...domain.ToolDefinition) *fake.ToolRegistry {
	return &fake.ToolRegistry{ListFunc: func(context.Context, int64, string, string) ([]domain.ToolDefinition, error) {
		return bound, nil
	}}
}

func TestBindingAuthorizerAllowsOnlyTheBoundToolVersion(t *testing.T) {
	search := domain.ToolDefinition{ToolID: "search", Version: "v1"}
	authorizer := &BindingAuthorizer{Registry: boundRegistry(search)}
	if err := authorizer.Authorize(context.Background(), validToolCall(), search); err != nil {
		t.Fatal(err)
	}
	if err := authorizer.Authorize(context.Background(), validToolCall(), domain.ToolDefinition{ToolID: "search", Version: "v2"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want a different version rejected", err)
	}
}

func TestBindingAuthorizerRejectsAnUnboundToolTheTenantHolds(t *testing.T) {
	authorizer := &BindingAuthorizer{Registry: boundRegistry(domain.ToolDefinition{ToolID: "search", Version: "v1"})}
	err := authorizer.Authorize(context.Background(), validToolCall(), domain.ToolDefinition{ToolID: "delete", Version: "v1"})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want tenant ownership alone to grant nothing", err)
	}
}

func TestBindingAuthorizerRejectsAnUnpinnedOrHumanPrincipal(t *testing.T) {
	authorizer := &BindingAuthorizer{Registry: boundRegistry()}
	unpinned := validToolCall()
	unpinned.Principal.Release = ""
	if err := authorizer.Authorize(context.Background(), unpinned, domain.ToolDefinition{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want an unpinned principal rejected", err)
	}
	human := validToolCall()
	human.Principal.Kind = domain.PrincipalHuman
	if err := authorizer.Authorize(context.Background(), human, domain.ToolDefinition{}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want a human principal rejected", err)
	}
}

func allowingPolicy(obligations ...domain.Obligation) fake.PolicyDecisionPointFunc {
	return func(context.Context, domain.DecisionSubject) (domain.Decision, error) {
		return domain.Decision{Outcome: domain.DecisionAllow, Obligations: obligations, PolicyID: "p", PolicyVersion: "1"}, nil
	}
}

func TestGatewayFailsOnAnObligationWithNoEnforcer(t *testing.T) {
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		t.Fatal("transport must not be called")
		return domain.ToolResult{}, nil
	}))
	gateway.Policy = allowingPolicy(domain.Obligation{Kind: domain.ObligationRequireApproval})
	// Ignoring an obligation would turn a conditional allow into an unconditional one.
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrDegraded) {
		t.Fatalf("error = %v, want an unenforceable obligation to fail the call", err)
	}
}

func TestGatewayAppliesObligationsBeforeEgress(t *testing.T) {
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		calls = append(calls, "transport")
		return domain.ToolResult{Output: []byte("ok")}, nil
	}))
	gateway.Policy = allowingPolicy(domain.Obligation{Kind: domain.ObligationRecordEvidence})
	gateway.Obligations = []contract.ObligationEnforcer{&fake.ObligationEnforcer{
		ObligationKind: domain.ObligationRecordEvidence,
		EnforceFunc: func(context.Context, domain.DecisionSubject, domain.Obligation) error {
			calls = append(calls, "obligation")
			return nil
		},
	}}
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err != nil {
		t.Fatal(err)
	}
	if index(calls, "obligation") > index(calls, "egress") {
		t.Fatalf("calls = %v, want obligations applied before egress", calls)
	}
}

func TestGatewayDeniedPolicyStopsBeforeGuardrails(t *testing.T) {
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		t.Fatal("transport must not be called")
		return domain.ToolResult{}, nil
	}))
	gateway.Policy = fake.PolicyDecisionPointFunc(func(context.Context, domain.DecisionSubject) (domain.Decision, error) {
		return domain.Decision{Outcome: domain.DecisionDeny, Reason: "no statement grants tool:invoke"}, nil
	})
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v, want a policy denial", err)
	}
}

func index(calls []string, want string) int {
	for i, call := range calls {
		if call == want {
			return i
		}
	}
	return len(calls)
}
