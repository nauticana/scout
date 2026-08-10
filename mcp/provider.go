package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// ToolProvider is one named MCP tool.
type ToolProvider interface {
	Name() string
	Definition() mcpgo.Tool
	Handle(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
}

// ResourceProvider is one browsable MCP resource.
type ResourceProvider interface {
	URI() string
	Definition() mcpgo.Resource
	Read(ctx context.Context) (any, error)
}
