package toolgateway

import (
	"fmt"
	"time"

	"github.com/nauticana/scout/contract"
)

// GovernedGatewayConfig is the validated composition input for NewGovernedGateway.
// Product-specific policy (registry, authorizer, credentials, egress, transport) is always injected;
// the breaker, retry policy, and failure classifier are constructed from configuration when omitted.
type GovernedGatewayConfig struct {
	Registry    contract.ToolRegistry
	RateLimiter contract.TenantRateLimiter
	Authorizer  contract.ToolAuthorizer
	Credentials contract.ToolCredentialProvider
	Egress      contract.ToolEgressPolicy
	Transport   contract.ToolTransport
	Validator   contract.ToolResultValidator
	// Circuit overrides the breaker built from Breaker; injection stays explicit.
	Circuit contract.ToolCircuitBreaker
	Breaker CircuitBreakerConfig
	// Retry overrides the policy built from RetryAttempts, RetryBaseDelay, and RetryMaxDelay.
	Retry          contract.ToolRetryPolicy
	RetryAttempts  int
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	Classifier     contract.ToolFailureClassifier
	// Guardrails is optional; when set, GuardrailConfigs must resolve the pinned policy per call.
	Guardrails       contract.GuardrailEnforcer
	GuardrailConfigs contract.ToolGuardrailConfigResolver
	Timeout          time.Duration
}

// NewGovernedGateway validates the composition and fills in the default breaker and retry policy.
func NewGovernedGateway(config GovernedGatewayConfig) (*GovernedGateway, error) {
	if config.Timeout <= 0 {
		return nil, fmt.Errorf("tool gateway: timeout must be positive")
	}
	circuit := config.Circuit
	if circuit == nil {
		breakerConfig := config.Breaker
		if breakerConfig.FailureThreshold == 0 && breakerConfig.Window == 0 && breakerConfig.OpenDuration == 0 && breakerConfig.MaxEntries == 0 {
			now := breakerConfig.Now
			breakerConfig = DefaultCircuitBreakerConfig()
			breakerConfig.Now = now
		}
		breaker, err := NewCircuitBreaker(breakerConfig)
		if err != nil {
			return nil, err
		}
		circuit = breaker
	}
	retry := config.Retry
	if retry == nil {
		if config.RetryAttempts <= 0 || config.RetryBaseDelay < 0 || config.RetryMaxDelay < 0 {
			return nil, fmt.Errorf("tool gateway: retry attempts must be positive and delays non-negative")
		}
		if config.RetryMaxDelay > 0 && config.RetryMaxDelay < config.RetryBaseDelay {
			return nil, fmt.Errorf("tool gateway: retry max delay cannot be below the base delay")
		}
		retry = RetryPolicy{MaxAttempts: config.RetryAttempts, BaseDelay: config.RetryBaseDelay, MaxDelay: config.RetryMaxDelay}
	}
	gateway := &GovernedGateway{
		Registry: config.Registry, RateLimiter: config.RateLimiter, Authorizer: config.Authorizer,
		Credentials: config.Credentials, Egress: config.Egress, Circuit: circuit,
		Transport: config.Transport, Retry: retry, Validator: config.Validator,
		Guardrails: config.Guardrails, GuardrailConfigs: config.GuardrailConfigs,
		Classifier: config.Classifier, Timeout: config.Timeout,
	}
	if err := gateway.validate(validationProbe); err != nil {
		return nil, err
	}
	return gateway, nil
}
