package toolgateway

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
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

func guardedGateway(t *testing.T, calls *[]string, enforcer *fake.GuardrailEnforcer, transport fake.ToolTransportFunc) *GovernedGateway {
	t.Helper()
	gateway := governedGateway(calls, transport)
	gateway.Guardrails = enforcer
	gateway.GuardrailConfigs = fake.ToolGuardrailConfigResolverFunc(func(context.Context, domain.ToolCall) (domain.GuardrailConfig, error) {
		*calls = append(*calls, "guardrail_config")
		return domain.GuardrailConfig{Version: "policy-1"}, nil
	})
	return gateway
}

func TestGovernedGatewayEnforcesGuardrailsAroundTheCall(t *testing.T) {
	var calls []string
	enforcer := &fake.GuardrailEnforcer{
		BeforeToolFunc: func(_ context.Context, config domain.GuardrailConfig, call domain.ToolCall) (domain.ToolCall, error) {
			calls = append(calls, "guardrail_before")
			if config.Version != "policy-1" {
				t.Fatalf("config = %+v", config)
			}
			call.Arguments = []byte("sanitized")
			return call, nil
		},
		AfterToolFunc: func(_ context.Context, _ domain.GuardrailConfig, result domain.ToolResult) (domain.ToolResult, error) {
			calls = append(calls, "guardrail_after")
			result.Output = []byte("guarded")
			return result, nil
		},
	}
	gateway := guardedGateway(t, &calls, enforcer, fake.ToolTransportFunc(func(_ context.Context, call domain.ToolCall, _ domain.ToolDefinition, _ []byte, _ time.Duration) (domain.ToolResult, error) {
		calls = append(calls, "transport")
		if string(call.Arguments) != "sanitized" {
			t.Fatalf("transport saw arguments %q", call.Arguments)
		}
		return domain.ToolResult{Output: []byte("raw")}, nil
	}))
	result, err := gateway.Invoke(context.Background(), validToolCall())
	if err != nil || string(result.Output) != "guarded" {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	want := []string{"rate", "registry", "authorize", "guardrail_config", "guardrail_before", "egress", "circuit_allow", "credential", "transport", "validate", "circuit_success", "guardrail_after"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestGovernedGatewayBlockedArgumentsNeverReachEgressOrTransport(t *testing.T) {
	var calls []string
	blocked := fmt.Errorf("%w: blocked by policy", domain.ErrForbidden)
	enforcer := &fake.GuardrailEnforcer{BeforeToolFunc: func(context.Context, domain.GuardrailConfig, domain.ToolCall) (domain.ToolCall, error) {
		return domain.ToolCall{}, blocked
	}}
	gateway := guardedGateway(t, &calls, enforcer, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		t.Fatal("transport must not be called")
		return domain.ToolResult{}, nil
	}))
	_, err := gateway.Invoke(context.Background(), validToolCall())
	if !errors.Is(err, blocked) || !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("error = %v", err)
	}
	for _, call := range calls {
		if call == "egress" || call == "credential" || call == "circuit_allow" {
			t.Fatalf("calls reached %s: %v", call, calls)
		}
	}
}

func TestGovernedGatewayRequiresGuardrailConfigResolver(t *testing.T) {
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		return domain.ToolResult{}, nil
	}))
	gateway.Guardrails = &fake.GuardrailEnforcer{}
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err == nil {
		t.Fatal("expected a wiring error")
	}
}

func TestGovernedGatewaySkipsBreakerForNonDependencyFailure(t *testing.T) {
	var calls []string
	rejected := fmt.Errorf("%w: bad arguments", domain.ErrValidation)
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		return domain.ToolResult{}, rejected
	}))
	gateway.Retry = RetryPolicy{MaxAttempts: 1}
	if _, err := gateway.Invoke(context.Background(), validToolCall()); !errors.Is(err, rejected) {
		t.Fatalf("error = %v", err)
	}
	for _, call := range calls {
		if call == "circuit_failure" {
			t.Fatalf("tenant input error reached the breaker: %v", calls)
		}
	}
}

func TestGovernedGatewayUsesFencedBreakerWhenAvailable(t *testing.T) {
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte("ok")}, nil
	}))
	var settled int64
	gateway.Circuit = &fake.FencedToolCircuitBreaker{
		AdmitFunc: func(context.Context, domain.ToolCall, domain.ToolDefinition) (int64, error) {
			calls = append(calls, "admit")
			return 42, nil
		},
		SettleFunc: func(_ context.Context, _ domain.ToolCall, _ domain.ToolDefinition, generation int64, success bool) error {
			calls = append(calls, "settle")
			settled = generation
			if !success {
				t.Fatal("expected a success settlement")
			}
			return nil
		},
	}
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err != nil || settled != 42 {
		t.Fatalf("settled = %d, error = %v", settled, err)
	}
}

func TestNewGovernedGatewayBuildsDefaults(t *testing.T) {
	var calls []string
	base := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		return domain.ToolResult{Output: []byte("ok")}, nil
	}))
	config := GovernedGatewayConfig{
		Registry: base.Registry, RateLimiter: base.RateLimiter, Authorizer: base.Authorizer,
		Credentials: base.Credentials, Egress: base.Egress, Transport: base.Transport,
		Validator: base.Validator, RetryAttempts: 2, RetryBaseDelay: time.Millisecond, Timeout: time.Second,
	}
	gateway, err := NewGovernedGateway(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gateway.Circuit.(*CircuitBreaker); !ok {
		t.Fatalf("circuit = %T, want the default breaker", gateway.Circuit)
	}
	if _, err := gateway.Invoke(context.Background(), validToolCall()); err != nil {
		t.Fatal(err)
	}
	invalid := config
	invalid.Timeout = 0
	if _, err := NewGovernedGateway(invalid); err == nil {
		t.Fatal("expected a timeout validation error")
	}
	missing := config
	missing.Validator = nil
	if _, err := NewGovernedGateway(missing); err == nil {
		t.Fatal("expected a dependency validation error")
	}
	noRetry := config
	noRetry.RetryAttempts = 0
	if _, err := NewGovernedGateway(noRetry); err == nil {
		t.Fatal("expected a retry validation error")
	}
	guarded := config
	guarded.Guardrails = &fake.GuardrailEnforcer{}
	if _, err := NewGovernedGateway(guarded); err == nil {
		t.Fatal("expected a guardrail resolver validation error")
	}
}
