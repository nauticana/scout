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

// ToolFunc handles one tool call.
type ToolFunc func(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)

// ReadFunc reads one resource payload for JSON projection.
type ReadFunc func(ctx context.Context) (any, error)

// Tool binds a definition to its handler, so a mismatch is a visible edit
// rather than a silent positional shift.
func Tool(definition mcpgo.Tool, handle ToolFunc) ToolProvider {
	return boundTool{definition: definition, handle: handle}
}

// Resource binds a definition to its read function.
func Resource(definition mcpgo.Resource, read ReadFunc) ResourceProvider {
	return boundResource{definition: definition, read: read}
}

type boundTool struct {
	definition mcpgo.Tool
	handle     ToolFunc
}

func (provider boundTool) Name() string           { return provider.definition.Name }
func (provider boundTool) Definition() mcpgo.Tool { return provider.definition }
func (provider boundTool) Handle(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return provider.handle(ctx, request)
}

type boundResource struct {
	definition mcpgo.Resource
	read       ReadFunc
}

func (provider boundResource) URI() string                           { return provider.definition.URI }
func (provider boundResource) Definition() mcpgo.Resource            { return provider.definition }
func (provider boundResource) Read(ctx context.Context) (any, error) { return provider.read(ctx) }

var (
	_ ToolProvider     = boundTool{}
	_ ResourceProvider = boundResource{}
)
