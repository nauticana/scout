package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func ResourceFunc(read func(context.Context) (any, error)) server.ResourceHandlerFunc {
	return func(ctx context.Context, request mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
		data, err := read(ctx)
		if err != nil {
			return nil, fmt.Errorf("resource handler: %w", err)
		}
		encoded, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("resource marshal: %w", err)
		}
		return []mcpgo.ResourceContents{mcpgo.TextResourceContents{
			URI: request.Params.URI, MIMEType: "application/json", Text: string(encoded),
		}}, nil
	}
}
