package mcp

import (
	"context"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// governed carries the caller resolution and result projection shared by the
// tool, resource, and prompt adapters.
type governed struct {
	callers   CallerResolver
	envelopes Envelopes
}

func (g governed) caller(ctx context.Context) (domain.MCPCaller, error) {
	if g.callers == nil {
		return domain.MCPCaller{}, fmt.Errorf("%w: no mcp caller resolver is configured", domain.ErrUnauthorized)
	}
	return g.callers.Resolve(ctx)
}

func requestID(ctx context.Context) string { return common.AsString(ctx.Value(common.RequestID)) }

// backendTool serves one catalog entry through a policy-checked backend call.
type backendTool struct {
	governed
	definition domain.MCPToolDefinition
	tool       mcpgo.Tool
	executor   contract.MCPToolExecutor
}

func (provider backendTool) Name() string           { return provider.definition.Name }
func (provider backendTool) Definition() mcpgo.Tool { return provider.tool }

func (provider backendTool) Handle(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, err := provider.caller(ctx)
	if err != nil {
		return WrapError(err), nil
	}
	if err = Authorize(provider.definition.Policy, caller); err != nil {
		return WrapError(err), nil
	}
	result, err := provider.executor.ExecuteTool(ctx, domain.MCPToolCall{
		Caller:    caller,
		RequestID: requestID(ctx),
		Name:      provider.definition.Name,
		Arguments: request.GetArguments(),
	})
	if err != nil {
		return WrapError(err), nil
	}
	return provider.envelopes.Result(result), nil
}

// backendResource reads URI-addressed product data for an authenticated caller.
type backendResource struct {
	governed
	reader contract.MCPResourceReader
}

func (provider backendResource) read(ctx context.Context, request mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	caller, err := provider.caller(ctx)
	if err != nil {
		return nil, err
	}
	contents, err := provider.reader.ReadResource(ctx, domain.MCPResourceRequest{
		Caller:    caller,
		RequestID: requestID(ctx),
		URI:       request.Params.URI,
	})
	if err != nil {
		return nil, fmt.Errorf("read resource %q: %w", request.Params.URI, err)
	}
	return contentsFrom(contents), nil
}

// backendPrompt renders client guidance and never executes a tool.
type backendPrompt struct {
	governed
	renderer contract.MCPPromptRenderer
}

func (provider backendPrompt) render(ctx context.Context, request mcpgo.GetPromptRequest) (*mcpgo.GetPromptResult, error) {
	caller, err := provider.caller(ctx)
	if err != nil {
		return nil, err
	}
	result, err := provider.renderer.RenderPrompt(ctx, domain.MCPPromptRequest{
		Caller:    caller,
		RequestID: requestID(ctx),
		Name:      request.Params.Name,
		Arguments: request.Params.Arguments,
	})
	if err != nil {
		return nil, fmt.Errorf("render prompt %q: %w", request.Params.Name, err)
	}
	return promptResultFrom(result), nil
}

var _ ToolProvider = backendTool{}
