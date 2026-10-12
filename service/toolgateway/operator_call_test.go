package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

var gatewayOperator = domain.Principal{Kind: domain.PrincipalHuman, ID: "9", TenantID: 7}

func operatorToolCall() domain.ToolCall {
	call := validToolCall()
	call.Operator = gatewayOperator
	call.OnBehalfOf = domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "end-user-5"}
	return call
}

// Decision 5: a person's call is authorized as the turn's principal (its release bindings and
// authority chain), decided by policy as that principal and as the person, runs on the person's
// own credential, is not governed by the agent's operating mode, and records the person.
func TestGatewayGovernsAnOperatorCallByTheIntersection(t *testing.T) {
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte(`{}`)}, nil
	}))
	var authorized []string
	gateway.Authorizer = fake.ToolAuthorizerFunc(func(_ context.Context, call domain.ToolCall, _ domain.ToolDefinition) error {
		authorized = append(authorized, call.Principal.ID)
		return nil
	})
	var decided []string
	deny := ""
	gateway.Policy = fake.PolicyDecisionPointFunc(func(_ context.Context, subject domain.DecisionSubject) (domain.Decision, error) {
		decided = append(decided, subject.Principal.ID)
		if subject.Principal.ID == deny {
			return domain.Decision{Outcome: domain.DecisionDeny, Reason: "not granted"}, nil
		}
		return domain.Decision{Outcome: domain.DecisionAllow}, nil
	})
	var credentialFor []string
	gateway.Credentials = fake.ToolCredentialProviderFunc(func(_ context.Context, principal domain.Principal, _, _, _ string) ([]byte, domain.AuthorityRef, error) {
		credentialFor = append(credentialFor, string(principal.Kind)+":"+principal.ID)
		return []byte("secret"), domain.AuthorityRef{}, nil
	})
	gateway.Limits = &fake.PrincipalLimits{AutonomyModeFunc: func(context.Context, domain.Principal, time.Time) (domain.AutonomyMode, error) {
		return domain.AutonomyHumanOnly, nil
	}}
	var recorded []domain.DecisionRecord
	gateway.Audit = &fake.AuditSink{RecordFunc: func(_ context.Context, record domain.DecisionRecord) error {
		recorded = append(recorded, record)
		return nil
	}}
	if _, err := gateway.Invoke(context.Background(), operatorToolCall()); err != nil {
		t.Fatalf("operator call = %v", err)
	}
	if !slices.Equal(authorized, []string{"agent"}) || !slices.Equal(decided, []string{"agent", "9"}) || !slices.Equal(credentialFor, []string{"human:9"}) {
		t.Fatalf("authorized %v, decided %v, credential for %v", authorized, decided, credentialFor)
	}
	for _, record := range recorded {
		if record.Principal != (domain.PrincipalRef{Kind: domain.PrincipalHuman, ID: "9"}) {
			t.Fatalf("record names %+v, not the person", record.Principal)
		}
	}
	// Outside the intersection: policy refuses the person, or the agent, or the agent's bindings.
	for name, refuse := range map[string]func(){
		"person's policy": func() { deny = "9" },
		"agent's policy":  func() { deny = "agent" },
		"agent's binding": func() {
			deny = ""
			gateway.Authorizer = fake.ToolAuthorizerFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition) error {
				return fmt.Errorf("%w: release does not bind the tool", domain.ErrForbidden)
			})
		},
	} {
		refuse()
		if _, err := gateway.Invoke(context.Background(), operatorToolCall()); !errors.Is(err, domain.ErrForbidden) || !errors.Is(err, domain.ErrNotSent) {
			t.Fatalf("%s = %v, want a refusal before the call left", name, err)
		}
	}
	// An operator of another tenant, or not a person, never calls.
	for _, operator := range []domain.Principal{{Kind: domain.PrincipalHuman, ID: "9", TenantID: 8}, {Kind: domain.PrincipalAgent, ID: "x", TenantID: 7}} {
		call := operatorToolCall()
		call.Operator = operator
		if _, err := gateway.Invoke(context.Background(), call); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("operator %+v = %v", operator, err)
		}
	}
}
