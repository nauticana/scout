package contract

import (
	"context"
	"time"

	"github.com/nauticana/scout/domain"
)

// GovernedToolGateway is the sole runtime path for external tool calls.
type GovernedToolGateway interface {
	// Invoke executes a tool through tenant authorization and resilience controls.
	Invoke(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error)
}

// ToolAuthorizer enforces tenant and agent permissions for tool calls.
type ToolAuthorizer interface {
	// Authorize verifies that the tenant, agent, and conversation may invoke a tool.
	Authorize(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) error
}

// ToolCredentialProvider supplies scoped, short-lived tool credentials.
type ToolCredentialProvider interface {
	// Credential returns a short-lived credential for an authorized tool call.
	Credential(ctx context.Context, tenantID int64, toolID string) ([]byte, error)
}

// ToolEgressPolicy restricts tool calls to approved destinations.
type ToolEgressPolicy interface {
	// ValidateDestination enforces the tenant's network egress allowlist.
	ValidateDestination(ctx context.Context, tenantID int64, endpoint string) error
}

// ToolTransport invokes approved tools within a bounded timeout.
type ToolTransport interface {
	// Invoke performs one bounded network call to an approved tool destination.
	Invoke(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, credential []byte, timeout time.Duration) (domain.ToolResult, error)
}

// ToolRetryPolicy governs bounded retries for tool failures.
type ToolRetryPolicy interface {
	// NextDelay returns the next retry delay and whether another attempt is allowed.
	NextDelay(ctx context.Context, call domain.ToolCall, result domain.ToolResult, callErr error, attempt int) (time.Duration, bool)
}

// ToolCircuitBreaker isolates failing tools by tenant.
type ToolCircuitBreaker interface {
	// Allow reports whether a tenant-scoped tool dependency may receive a call.
	Allow(ctx context.Context, tenantID int64, toolID string) error
	// RecordSuccess records a successful tenant-scoped tool call.
	RecordSuccess(ctx context.Context, tenantID int64, toolID string) error
	// RecordFailure records a failed tenant-scoped tool call.
	RecordFailure(ctx context.Context, tenantID int64, toolID string) error
}

// ToolResultValidator enforces registered tool output contracts.
type ToolResultValidator interface {
	// Validate checks a tool response against its registered output contract.
	Validate(ctx context.Context, definition domain.ToolDefinition, result domain.ToolResult) error
}

// FencedToolCircuitBreaker is the optional capability that ties every outcome to the generation
// that admitted the call, so a stale outcome can never move newer breaker state, and that
// tracks shared dependency health by destination when configured.
type FencedToolCircuitBreaker interface {
	ToolCircuitBreaker
	// Admit is Allow with an admission generation token.
	Admit(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition) (generation int64, err error)
	// Settle records the outcome of an admitted call; outcomes from older generations are ignored.
	Settle(ctx context.Context, call domain.ToolCall, definition domain.ToolDefinition, generation int64, success bool) error
}

// ToolFailureClassifier decides whether one failed attempt reflects dependency health;
// caller cancellation, tenant input errors, and authorization rejections must never trip a breaker.
type ToolFailureClassifier interface {
	CountsAsDependencyFailure(ctx context.Context, call domain.ToolCall, result domain.ToolResult, err error) bool
}

// ToolGuardrailConfigResolver returns the pinned guardrail policy governing one tool call.
type ToolGuardrailConfigResolver interface {
	GuardrailConfig(ctx context.Context, call domain.ToolCall) (domain.GuardrailConfig, error)
}
