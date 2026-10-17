package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ToolSchemaToolName is the read tool a server registers with OmitOutputSchemas
// to answer one backend tool's input and output schemas.
const ToolSchemaToolName = "get_tool_schema"

// toolSchemaTool answers with a backend tool's schemas exactly as tools/list
// would carry them without the omission, only for a caller that may call the
// tool. Its own answer, {name, input_schema, output_schema}, is structured
// content without a declared schema, like every tool on such a server.
type toolSchemaTool struct {
	governed
	catalog contract.MCPToolCatalog
	// schemas holds each backend tool's encoded answer.
	schemas map[string]json.RawMessage
}

func newToolSchemaTool(g governed, catalog contract.MCPToolCatalog, tools []mcpgo.Tool) (toolSchemaTool, error) {
	provider := toolSchemaTool{governed: g, catalog: catalog, schemas: make(map[string]json.RawMessage, len(tools))}
	for _, tool := range tools {
		encoded, err := json.Marshal(tool)
		if err != nil {
			return provider, fmt.Errorf("mcp tool %q: %w", tool.Name, err)
		}
		var entry struct {
			InputSchema  json.RawMessage `json:"inputSchema"`
			OutputSchema json.RawMessage `json:"outputSchema"`
		}
		if err = json.Unmarshal(encoded, &entry); err != nil {
			return provider, fmt.Errorf("mcp tool %q: %w", tool.Name, err)
		}
		if len(entry.OutputSchema) == 0 {
			entry.OutputSchema = json.RawMessage(`{}`)
		}
		answer, err := json.Marshal(map[string]any{"name": tool.Name, "input_schema": entry.InputSchema, "output_schema": entry.OutputSchema})
		if err != nil {
			return provider, fmt.Errorf("mcp tool %q: %w", tool.Name, err)
		}
		provider.schemas[tool.Name] = answer
	}
	return provider, nil
}

func (provider toolSchemaTool) Name() string { return ToolSchemaToolName }

func (provider toolSchemaTool) Definition() mcpgo.Tool {
	return mcpgo.NewTool(ToolSchemaToolName,
		mcpgo.WithTitleAnnotation("Tool schema"),
		mcpgo.WithDescription("The input and output JSON Schemas of one tool by name. tools/list leaves output schemas out; read a tool's here before relying on the shape of its result."),
		mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Tool name from tools/list.")),
		mcpgo.WithReadOnlyHintAnnotation(true),
		mcpgo.WithDestructiveHintAnnotation(false),
		mcpgo.WithIdempotentHintAnnotation(true),
		mcpgo.WithOpenWorldHintAnnotation(false),
	)
}

func (provider toolSchemaTool) Handle(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	name, failed := RequireString(request, "name")
	if failed != nil {
		return failed, nil
	}
	caller, err := provider.caller(ctx)
	if err != nil {
		return WrapError(err), nil
	}
	answer, known := provider.schemas[name]
	if !known || admitted(ctx, provider.catalog, caller, name) != nil {
		return WrapError(fmt.Errorf("%w: tool %q", domain.ErrNotFound, name)), nil
	}
	wrapped := provider.envelopes.Wrap(answer, nil)
	wrapped.StructuredContent = answer
	return wrapped, nil
}

var _ ToolProvider = toolSchemaTool{}
