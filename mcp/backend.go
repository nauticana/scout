package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/nauticana/keel/common"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
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
	caller, err := g.callers.Resolve(ctx)
	if err != nil {
		return caller, err
	}
	caller.ElicitForm, caller.ElicitURL = clientElicitation(ctx)
	return caller, nil
}

func requestID(ctx context.Context) string { return common.AsString(ctx.Value(common.RequestID)) }

// backendTool serves one catalog entry through a policy-checked backend call.
type backendTool struct {
	governed
	definition domain.MCPToolDefinition
	tool       mcpgo.Tool
	executor   contract.MCPToolExecutor
	output     *jsonschema.Schema
	onDenied   DeniedFunc
}

func (provider backendTool) Name() string           { return provider.definition.Name }
func (provider backendTool) Definition() mcpgo.Tool { return provider.tool }

func (provider backendTool) Handle(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	caller, err := provider.caller(ctx)
	if err != nil {
		if provider.onDenied != nil {
			provider.onDenied(ctx, caller, provider.definition.Name, err)
		}
		return WrapError(err), nil
	}
	if err = Authorize(provider.definition.Policy, caller); err != nil {
		if provider.onDenied != nil {
			provider.onDenied(ctx, caller, provider.definition.Name, err)
		}
		return WrapError(err), nil
	}
	elicited, err := elicitedFrom(request.Params.InputResponses)
	if err != nil {
		return WrapError(err), nil
	}
	result, err := provider.executor.ExecuteTool(ctx, domain.MCPToolCall{
		Caller:    caller,
		RequestID: requestID(ctx),
		Name:      provider.definition.Name,
		Arguments: request.GetArguments(),
		Elicited:  elicited,
		State:     request.Params.RequestState,
	})
	if err != nil {
		return WrapError(err), nil
	}
	if result.Error != nil {
		if result.Data != nil || result.Meta != nil || len(result.Evidence) > 0 || result.Task != nil || len(result.Elicit) > 0 || result.State != "" {
			return WrapError(fmt.Errorf("%w: tool %q returned an error with other result fields", domain.ErrContractFailed, provider.definition.Name)), nil
		}
		return WrapToolError(*result.Error), nil
	}
	if len(result.Elicit) > 0 {
		asked, err := elicitationResult(result, caller)
		if err != nil {
			return WrapError(err), nil
		}
		return asked, nil
	}
	wrapped := provider.envelopes.Result(result)
	if provider.output == nil || wrapped.IsError {
		return wrapped, nil
	}
	encoded, err := json.Marshal(result.Data)
	if err == nil {
		err = provider.output.ValidateJSON(encoded)
	}
	if err != nil {
		return WrapError(fmt.Errorf("%w: tool %q output: %w", domain.ErrContractFailed, provider.definition.Name, err)), nil
	}
	wrapped.StructuredContent = json.RawMessage(encoded)
	return wrapped, nil
}

// backendResource reads URI-addressed product data for an authenticated caller.
type backendResource struct {
	governed
	backend contract.MCPResourceBackend
	key     string
}

// authorize admits a read only of an entry in the caller's own catalog whose
// declared scopes the caller holds.
func (provider backendResource) authorize(ctx context.Context, caller domain.MCPCaller) error {
	definitions, err := provider.backend.ListResources(ctx, caller)
	if err != nil {
		return fmt.Errorf("mcp resource catalog: %w", err)
	}
	for _, definition := range definitions {
		if resourceKey(definition) == provider.key {
			return requireScopes(definition.Policy.RequiredScopes, caller)
		}
	}
	return fmt.Errorf("%w: resource %q", domain.ErrNotFound, provider.key)
}

func (provider backendResource) read(ctx context.Context, request mcpgo.ReadResourceRequest) ([]mcpgo.ResourceContents, error) {
	caller, err := provider.caller(ctx)
	if err != nil {
		return nil, err
	}
	if err = provider.authorize(ctx, caller); err != nil {
		return nil, err
	}
	contents, err := provider.backend.ReadResource(ctx, domain.MCPResourceRequest{
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
