package domain

// ToolDefinition is an immutable tenant tool contract.
type ToolDefinition struct {
	ToolID   string
	Version  string
	Endpoint string
	Contract []byte
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
}

// ToolResult contains validated output and usage from a tool.
type ToolResult struct {
	Output    []byte
	Retryable bool
	Usage     Usage
}
