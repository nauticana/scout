package mcp

import (
	"encoding/base64"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/nauticana/scout/domain"
)

// Projections from SDK-neutral definitions onto mcp-go protocol values. The
// protocol library owns the wire frame; Scout owns everything it carries.

func toolFrom(definition domain.MCPToolDefinition) mcpgo.Tool {
	tool := mcpgo.NewTool(definition.Name, mcpgo.WithDescription(definition.Description))
	if len(definition.InputSchema) > 0 {
		tool = mcpgo.NewToolWithRawSchema(definition.Name, definition.Description, definition.InputSchema)
	}
	tool.Annotations = mcpgo.ToolAnnotation(definition.Annotations)
	tool.RawOutputSchema = definition.OutputSchema
	return tool
}

func resourceFrom(definition domain.MCPResourceDefinition) mcpgo.Resource {
	return mcpgo.NewResource(definition.URI, definition.Name,
		mcpgo.WithResourceDescription(describe(definition.Title, definition.Description)),
		mcpgo.WithMIMEType(definition.MIMEType))
}

func resourceTemplateFrom(definition domain.MCPResourceDefinition) mcpgo.ResourceTemplate {
	return mcpgo.NewResourceTemplate(definition.URITemplate, definition.Name,
		mcpgo.WithTemplateDescription(describe(definition.Title, definition.Description)),
		mcpgo.WithTemplateMIMEType(definition.MIMEType))
}

func promptFrom(definition domain.MCPPromptDefinition) mcpgo.Prompt {
	arguments := make([]mcpgo.PromptArgument, len(definition.Arguments))
	for i, argument := range definition.Arguments {
		arguments[i] = mcpgo.PromptArgument(argument)
	}
	return mcpgo.Prompt{
		Name:        definition.Name,
		Description: describe(definition.Title, definition.Description),
		Arguments:   arguments,
	}
}

func contentsFrom(contents []domain.MCPResourceContent) []mcpgo.ResourceContents {
	projected := make([]mcpgo.ResourceContents, len(contents))
	for i, content := range contents {
		if len(content.Blob) > 0 {
			projected[i] = mcpgo.BlobResourceContents{
				URI: content.URI, MIMEType: content.MIMEType,
				Blob: base64.StdEncoding.EncodeToString(content.Blob),
			}
			continue
		}
		projected[i] = mcpgo.TextResourceContents{URI: content.URI, MIMEType: content.MIMEType, Text: content.Text}
	}
	return projected
}

func promptResultFrom(result domain.MCPPromptResult) *mcpgo.GetPromptResult {
	messages := make([]mcpgo.PromptMessage, len(result.Messages))
	for i, message := range result.Messages {
		messages[i] = mcpgo.NewPromptMessage(mcpgo.Role(message.Role), mcpgo.NewTextContent(message.Text))
	}
	return mcpgo.NewGetPromptResult(result.Description, messages)
}

// describe falls back to the title where mcp-go carries only a description.
func describe(title, description string) string {
	if description == "" {
		return title
	}
	return description
}
