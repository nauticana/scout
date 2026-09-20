package domain

import "time"

// ToolDefinition is an immutable tenant tool contract.
type ToolDefinition struct {
	ToolID       string
	Version      string
	DisplayName  string
	Endpoint     string
	InputSchema  []byte
	OutputSchema []byte
	Timeout      time.Duration
	MaxAttempts  int
	// VerifyEffect marks a mutating tool whose call succeeds only once its effect is observed.
	VerifyEffect bool
	// RetryWhenEffectAbsent declares that a call whose effect is proven absent may be sent again
	// under the same idempotency key, within MaxAttempts; it requires VerifyEffect.
	RetryWhenEffectAbsent bool
}

// ToolReference names one registered tool version.
type ToolReference struct {
	ToolID  string `json:"tool_id"`
	Version string `json:"version"`
}

// ToolCall contains one governed tenant tool invocation. Principal is the agent
// making the call; the gateway rejects a zero principal.
type ToolCall struct {
	TenantContext TenantContext
	Principal     Principal
	// OnBehalfOf is the human the turn acts for, so a handler can authorize per user; zero when there is none.
	OnBehalfOf     PrincipalRef
	RequestID      string
	ConversationID string
	ToolID         string
	ToolVersion    string
	Arguments      []byte
	// IdempotencyKey is stable across redelivery of the same proposed call, so a
	// transport can refuse to repeat a committed effect.
	IdempotencyKey string
}

// ToolResult contains validated output and usage from a tool.
type ToolResult struct {
	Output    []byte
	Retryable bool
	Usage     Usage
	// Evidence are the resource links an MCP-backed tool returned with its output.
	Evidence []MCPResourceLink
	// Effect is the observed postcondition of a tool that declares VerifyEffect.
	Effect *EffectObservation
}
