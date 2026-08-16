package fake

import (
	"context"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ToolRegistry contains configurable immutable tool operations.
type ToolRegistry struct {
	RegisterFunc func(context.Context, int64, domain.ToolDefinition) error
	GetFunc      func(context.Context, int64, string, string) (domain.ToolDefinition, error)
	ListFunc     func(context.Context, int64, string, string) ([]domain.ToolDefinition, error)
}

// Register invokes RegisterFunc.
func (registry *ToolRegistry) Register(ctx context.Context, tenantID int64, tool domain.ToolDefinition) error {
	return registry.RegisterFunc(ctx, tenantID, tool)
}

// Get invokes GetFunc.
func (registry *ToolRegistry) Get(ctx context.Context, tenantID int64, toolID, version string) (domain.ToolDefinition, error) {
	return registry.GetFunc(ctx, tenantID, toolID, version)
}

// List invokes ListFunc.
func (registry *ToolRegistry) List(ctx context.Context, tenantID int64, agentID, agentVersion string) ([]domain.ToolDefinition, error) {
	return registry.ListFunc(ctx, tenantID, agentID, agentVersion)
}

// ToolAuthorizerFunc adapts a function to contract.ToolAuthorizer.
type ToolAuthorizerFunc func(context.Context, domain.ToolCall, domain.ToolDefinition) error

// Authorize invokes the configured function.
func (function ToolAuthorizerFunc) Authorize(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) error {
	return function(ctx, call, definition)
}

// ToolCredentialProviderFunc adapts a function to contract.ToolCredentialProvider.
type ToolCredentialProviderFunc func(context.Context, int64, string) ([]byte, error)

// Credential invokes the configured function.
func (function ToolCredentialProviderFunc) Credential(ctx context.Context, tenantID int64, toolID string) ([]byte, error) {
	return function(ctx, tenantID, toolID)
}

// ToolEgressPolicyFunc adapts a function to contract.ToolEgressPolicy.
type ToolEgressPolicyFunc func(context.Context, int64, string) error

// ValidateDestination invokes the configured function.
func (function ToolEgressPolicyFunc) ValidateDestination(ctx context.Context, tenantID int64, endpoint string) error {
	return function(ctx, tenantID, endpoint)
}

// ToolTransportFunc adapts a function to contract.ToolTransport.
type ToolTransportFunc func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error)

// Invoke invokes the configured function.
func (function ToolTransportFunc) Invoke(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, credential []byte, timeout time.Duration) (domain.ToolResult, error) {
	return function(ctx, call, definition, credential, timeout)
}

// ToolCircuitBreaker contains configurable circuit callbacks.
type ToolCircuitBreaker struct {
	AllowFunc         func(context.Context, int64, string) error
	RecordSuccessFunc func(context.Context, int64, string) error
	RecordFailureFunc func(context.Context, int64, string) error
}

// Allow invokes AllowFunc.
func (breaker *ToolCircuitBreaker) Allow(ctx context.Context, tenantID int64, toolID string) error {
	return breaker.AllowFunc(ctx, tenantID, toolID)
}

// RecordSuccess invokes RecordSuccessFunc.
func (breaker *ToolCircuitBreaker) RecordSuccess(ctx context.Context, tenantID int64, toolID string) error {
	return breaker.RecordSuccessFunc(ctx, tenantID, toolID)
}

// RecordFailure invokes RecordFailureFunc.
func (breaker *ToolCircuitBreaker) RecordFailure(ctx context.Context, tenantID int64, toolID string) error {
	return breaker.RecordFailureFunc(ctx, tenantID, toolID)
}

// ToolResultValidatorFunc adapts a function to contract.ToolResultValidator.
type ToolResultValidatorFunc func(context.Context, domain.ToolDefinition, domain.ToolResult) error

// Validate invokes the configured function.
func (function ToolResultValidatorFunc) Validate(ctx context.Context, definition domain.ToolDefinition, result domain.ToolResult) error {
	return function(ctx, definition, result)
}

// FencedToolCircuitBreaker contains configurable fenced circuit callbacks.
type FencedToolCircuitBreaker struct {
	ToolCircuitBreaker
	AdmitFunc  func(context.Context, domain.ToolCall, domain.ToolDefinition) (int64, error)
	SettleFunc func(context.Context, domain.ToolCall, domain.ToolDefinition, int64, bool) error
}

// Admit invokes AdmitFunc.
func (breaker *FencedToolCircuitBreaker) Admit(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) (int64, error) {
	return breaker.AdmitFunc(ctx, call, definition)
}

// Settle invokes SettleFunc.
func (breaker *FencedToolCircuitBreaker) Settle(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, generation int64, success bool) error {
	return breaker.SettleFunc(ctx, call, definition, generation, success)
}

// ToolFailureClassifierFunc adapts a function to contract.ToolFailureClassifier.
type ToolFailureClassifierFunc func(context.Context, domain.ToolCall, domain.ToolResult, error) bool

// CountsAsDependencyFailure invokes the configured function.
func (function ToolFailureClassifierFunc) CountsAsDependencyFailure(ctx context.Context, call domain.ToolCall, result domain.ToolResult, err error) bool {
	return function(ctx, call, result, err)
}

// ToolGuardrailConfigResolverFunc adapts a function to contract.ToolGuardrailConfigResolver.
type ToolGuardrailConfigResolverFunc func(context.Context, domain.ToolCall) (domain.GuardrailConfig, error)

// GuardrailConfig invokes the configured function.
func (function ToolGuardrailConfigResolverFunc) GuardrailConfig(ctx context.Context, call domain.ToolCall) (domain.GuardrailConfig, error) {
	return function(ctx, call)
}

var _ contract.ToolRegistry = (*ToolRegistry)(nil)
var _ contract.ToolAuthorizer = ToolAuthorizerFunc(nil)
var _ contract.ToolCredentialProvider = ToolCredentialProviderFunc(nil)
var _ contract.ToolEgressPolicy = ToolEgressPolicyFunc(nil)
var _ contract.ToolTransport = ToolTransportFunc(nil)
var _ contract.ToolCircuitBreaker = (*ToolCircuitBreaker)(nil)
var _ contract.ToolResultValidator = ToolResultValidatorFunc(nil)
var _ contract.FencedToolCircuitBreaker = (*FencedToolCircuitBreaker)(nil)
var _ contract.ToolFailureClassifier = ToolFailureClassifierFunc(nil)
var _ contract.ToolGuardrailConfigResolver = ToolGuardrailConfigResolverFunc(nil)
