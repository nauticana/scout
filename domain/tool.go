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
}

// ToolReference names one registered tool version.
type ToolReference struct {
	ToolID  string `json:"tool_id"`
	Version string `json:"version"`
}

// ToolCall contains one governed tenant tool invocation. Principal is the agent
// making the call; the gateway rejects a zero principal.
type ToolCall struct {
	TenantContext  TenantContext
	Principal      Principal
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
}
