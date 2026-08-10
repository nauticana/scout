package toolgateway

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/internal/fake"
)

func validToolCall() domain.ToolCall {
	return domain.ToolCall{
		TenantContext:  domain.TenantContext{TenantID: 7},
		RequestID:      "request",
		ConversationID: "conversation",
		ToolID:         "search",
		ToolVersion:    "v1",
	}
}

func governedGateway(calls *[]string, transport fake.ToolTransportFunc) *GovernedGateway {
	return &GovernedGateway{
		Registry: &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
			*calls = append(*calls, "registry")
			return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: "https://example.invalid/tool"}, nil
		}},
		RateLimiter: &fake.TenantRateLimiter{AllowToolCallFunc: func(context.Context, domain.ToolCall) error {
			*calls = append(*calls, "rate")
			return nil
		}},
		Authorizer: fake.ToolAuthorizerFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition) error {
			*calls = append(*calls, "authorize")
			return nil
		}),
		Credentials: fake.ToolCredentialProviderFunc(func(context.Context, int64, string) ([]byte, error) {
			*calls = append(*calls, "credential")
			return []byte("secret"), nil
		}),
		Egress: fake.ToolEgressPolicyFunc(func(context.Context, int64, string) error {
			*calls = append(*calls, "egress")
			return nil
		}),
		Circuit: &fake.ToolCircuitBreaker{
			AllowFunc: func(context.Context, int64, string) error {
				*calls = append(*calls, "circuit_allow")
				return nil
			},
			RecordSuccessFunc: func(context.Context, int64, string) error {
				*calls = append(*calls, "circuit_success")
				return nil
			},
			RecordFailureFunc: func(context.Context, int64, string) error {
				*calls = append(*calls, "circuit_failure")
				return nil
			},
		},
		Transport: transport,
		Retry:     RetryPolicy{MaxAttempts: 2},
		Validator: fake.ToolResultValidatorFunc(func(context.Context, domain.ToolDefinition, domain.ToolResult) error {
			*calls = append(*calls, "validate")
			return nil
		}),
		Timeout: time.Second,
	}
}

func TestGovernedGatewayAppliesCompleteChain(t *testing.T) {
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(_ context.Context, _ domain.ToolCall, _ domain.ToolDefinition, credential []byte, timeout time.Duration) (domain.ToolResult, error) {
		calls = append(calls, "transport")
		if string(credential) != "secret" || timeout != time.Second {
			t.Fatalf("credential = %q, timeout = %s", credential, timeout)
		}
		return domain.ToolResult{Output: []byte("ok")}, nil
	}))
	result, err := gateway.Invoke(context.Background(), validToolCall())
	if err != nil || string(result.Output) != "ok" {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	want := []string{"rate", "registry", "authorize", "egress", "circuit_allow", "credential", "transport", "validate", "circuit_success"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestGovernedGatewayRetriesBoundedFailure(t *testing.T) {
	var calls []string
	attempts := 0
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		calls = append(calls, "transport")
		attempts++
		if attempts == 1 {
			return domain.ToolResult{Retryable: true}, errors.New("temporary")
		}
		return domain.ToolResult{Output: []byte("ok")}, nil
	}))
	result, err := gateway.Invoke(context.Background(), validToolCall())
	if err != nil || string(result.Output) != "ok" || attempts != 2 {
		t.Fatalf("result = %+v, attempts = %d, error = %v", result, attempts, err)
	}
	wantTail := []string{"transport", "circuit_failure", "transport", "validate", "circuit_success"}
	if !reflect.DeepEqual(calls[len(calls)-len(wantTail):], wantTail) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestGovernedGatewayTreatsValidationFailureAsCircuitFailure(t *testing.T) {
	validationErr := errors.New("invalid result")
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		return domain.ToolResult{}, nil
	}))
	gateway.Validator = fake.ToolResultValidatorFunc(func(context.Context, domain.ToolDefinition, domain.ToolResult) error {
		return validationErr
	})
	gateway.Retry = RetryPolicy{MaxAttempts: 1}
	_, err := gateway.Invoke(context.Background(), validToolCall())
	if !errors.Is(err, validationErr) || calls[len(calls)-1] != "circuit_failure" {
		t.Fatalf("calls = %v, error = %v", calls, err)
	}
}

func TestGovernedGatewayStopsBeforeCredentialsWhenEgressFails(t *testing.T) {
	want := errors.New("blocked")
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		t.Fatal("transport must not be called")
		return domain.ToolResult{}, nil
	}))
	gateway.Egress = fake.ToolEgressPolicyFunc(func(context.Context, int64, string) error { return want })
	_, err := gateway.Invoke(context.Background(), validToolCall())
	if !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}
