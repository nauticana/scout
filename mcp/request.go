package mcp

import (
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func RequireInt(request mcpgo.CallToolRequest, name string) (int64, *mcpgo.CallToolResult) {
	value := int64(request.GetInt(name, 0))
	if value == 0 {
		return 0, WrapErrorf("%s is required", name)
	}
	return value, nil
}

func RequireString(request mcpgo.CallToolRequest, name string) (string, *mcpgo.CallToolResult) {
	value := request.GetString(name, "")
	if value == "" {
		return "", WrapErrorf("%s is required", name)
	}
	return value, nil
}

func WrapErrorf(format string, args ...any) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultError(fmt.Sprintf(format, args...))
}
