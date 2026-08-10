package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// MCPServerDescriber supplies product-owned values for Keel server configuration.
type MCPServerDescriber interface {
	// DescribeServer returns the stable server identity and instructions.
	DescribeServer() domain.MCPServerDefinition
}

// MCPToolCatalog lists tools visible to the authenticated caller.
type MCPToolCatalog interface {
	// ListTools returns SDK-neutral definitions with enforceable policy metadata.
	ListTools(ctx context.Context, caller domain.MCPCaller) ([]domain.MCPToolDefinition, error)
}

// MCPToolExecutor invokes product services after Keel infrastructure checks.
type MCPToolExecutor interface {
	// ExecuteTool performs one bounded call or returns a durable task reference.
	ExecuteTool(ctx context.Context, call domain.MCPToolCall) (domain.MCPToolResult, error)
}

// MCPToolBackend combines caller-specific discovery and governed execution.
type MCPToolBackend interface {
	MCPToolCatalog
	MCPToolExecutor
}

// MCPResourceCatalog lists resources visible to the authenticated caller.
type MCPResourceCatalog interface {
	// ListResources returns SDK-neutral resource definitions.
	ListResources(ctx context.Context, caller domain.MCPCaller) ([]domain.MCPResourceDefinition, error)
}

// MCPResourceReader reads product data without exposing provider SDK types.
type MCPResourceReader interface {
	// ReadResource returns one or more contents for the requested URI.
	ReadResource(ctx context.Context, request domain.MCPResourceRequest) ([]domain.MCPResourceContent, error)
}

// MCPResourceBackend combines caller-specific discovery and resource reads.
type MCPResourceBackend interface {
	MCPResourceCatalog
	MCPResourceReader
}

// MCPPromptCatalog lists client-guidance templates visible to the authenticated caller.
type MCPPromptCatalog interface {
	// ListPrompts returns SDK-neutral prompt definitions.
	ListPrompts(ctx context.Context, caller domain.MCPCaller) ([]domain.MCPPromptDefinition, error)
}

// MCPPromptRenderer renders client-guidance templates without invoking tools.
type MCPPromptRenderer interface {
	// RenderPrompt validates arguments and returns messages for client-controlled execution.
	RenderPrompt(ctx context.Context, request domain.MCPPromptRequest) (domain.MCPPromptResult, error)
}

// MCPPromptBackend combines caller-specific discovery and prompt rendering.
type MCPPromptBackend interface {
	MCPPromptCatalog
	MCPPromptRenderer
}
