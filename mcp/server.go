package mcp

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/jsonschema"
)

// ServerConfig contains product-owned MCP server identity and copy.
type ServerConfig struct {
	Name         string
	Version      string
	Instructions string
	Source       string
	ClientIPHook func(context.Context, *http.Request) context.Context
	Callers      CallerResolver
}

// BaseServer wraps mcp-go with name-keyed provider registration and
// caller-scoped discovery.
type BaseServer struct {
	mcp       *server.MCPServer
	ipHook    func(context.Context, *http.Request) context.Context
	source    string
	callers   CallerResolver
	tools     contract.MCPToolCatalog
	resources contract.MCPResourceCatalog
	// resourceKeys are the backend's entries; directly registered resources are not caller-scoped.
	resourceKeys []string
	prompts      contract.MCPPromptCatalog
}

func NewServer(config ServerConfig) *BaseServer {
	base := &BaseServer{ipHook: config.ClientIPHook, source: config.Source, callers: config.Callers}
	if base.ipHook == nil {
		base.ipHook = keelhandler.WithClientIPContext
	}
	if base.callers == nil {
		base.callers = BaseCallerResolver{}
	}
	// mcp-go has no resource filter, so listings are narrowed after the fact.
	hooks := &server.Hooks{}
	hooks.AddAfterListResources(func(ctx context.Context, _ any, _ *mcpgo.ListResourcesRequest, result *mcpgo.ListResourcesResult) {
		hidden := base.hiddenResources(ctx)
		result.Resources = slices.DeleteFunc(result.Resources, func(resource mcpgo.Resource) bool { return hidden[resource.URI] })
	})
	hooks.AddAfterListResourceTemplates(func(ctx context.Context, _ any, _ *mcpgo.ListResourceTemplatesRequest, result *mcpgo.ListResourceTemplatesResult) {
		hidden := base.hiddenResources(ctx)
		result.ResourceTemplates = slices.DeleteFunc(result.ResourceTemplates, func(template mcpgo.ResourceTemplate) bool {
			return template.URITemplate != nil && template.URITemplate.Template != nil && hidden[template.URITemplate.Raw()]
		})
	})
	base.mcp = server.NewMCPServer(
		config.Name, config.Version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithPromptCapabilities(true),
		server.WithInstructions(config.Instructions),
		server.WithRecovery(),
		server.WithToolFilter(base.visibleTools),
		server.WithPromptFilter(base.visiblePrompts),
		server.WithHooks(hooks),
	)
	return base
}

// NewServerFor builds a server from the product's own server description.
func NewServerFor(describer contract.MCPServerDescriber, callers CallerResolver) *BaseServer {
	definition := describer.DescribeServer()
	return NewServer(ServerConfig{
		Name:         definition.Name,
		Version:      definition.Version,
		Instructions: definition.Instructions,
		Source:       definition.Source,
		Callers:      callers,
	})
}

func (s *BaseServer) Envelopes() Envelopes { return NewEnvelopes(s.source) }

func (s *BaseServer) governed() governed {
	return governed{callers: s.callers, envelopes: s.Envelopes()}
}

func (s *BaseServer) Register(tools ...ToolProvider) {
	for _, tool := range tools {
		s.mcp.AddTool(tool.Definition(), tool.Handle)
	}
}

func (s *BaseServer) RegisterResource(resources ...ResourceProvider) {
	for _, resource := range resources {
		s.mcp.AddResource(resource.Definition(), ResourceFunc(resource.Read))
	}
}

// RegisterToolBackend publishes the full catalog and routes every call through
// the backend after scope authorization. Remote callers list only the subset
// the backend returns for them.
func (s *BaseServer) RegisterToolBackend(ctx context.Context, backend contract.MCPToolBackend) error {
	definitions, err := backend.ListTools(ctx, HostCaller())
	if err != nil {
		return fmt.Errorf("mcp tool catalog: %w", err)
	}
	outputs := make([]*jsonschema.Schema, len(definitions))
	for i, definition := range definitions {
		if outputs[i], err = outputSchema(definition); err != nil {
			return err
		}
	}
	s.tools = backend
	for i, definition := range definitions {
		s.Register(backendTool{
			governed:   s.governed(),
			definition: definition,
			tool:       toolFrom(definition),
			executor:   backend,
			output:     outputs[i],
		})
	}
	return nil
}

// outputSchema compiles a declared output schema with the validator that
// enforces it on every result; nil when the tool declares none.
func outputSchema(definition domain.MCPToolDefinition) (*jsonschema.Schema, error) {
	if len(definition.OutputSchema) == 0 {
		return nil, nil
	}
	schema, err := jsonschema.Compile(definition.OutputSchema)
	if err != nil {
		return nil, fmt.Errorf("%w: mcp tool %q output schema: %w", domain.ErrValidation, definition.Name, err)
	}
	if schema.Type != "object" {
		return nil, fmt.Errorf("%w: mcp tool %q output schema must have type object", domain.ErrValidation, definition.Name)
	}
	return schema, nil
}

// RegisterResourceBackend publishes catalog entries as fixed resources, or as
// URI templates when the entry carries one. Remote callers list and read only
// the entries the backend returns for them and whose scopes they hold.
func (s *BaseServer) RegisterResourceBackend(ctx context.Context, backend contract.MCPResourceBackend) error {
	definitions, err := backend.ListResources(ctx, HostCaller())
	if err != nil {
		return fmt.Errorf("mcp resource catalog: %w", err)
	}
	s.resources = backend
	for _, definition := range definitions {
		s.resourceKeys = append(s.resourceKeys, resourceKey(definition))
		reader := backendResource{governed: s.governed(), backend: backend, key: resourceKey(definition)}
		if definition.URITemplate != "" {
			s.mcp.AddResourceTemplate(resourceTemplateFrom(definition), reader.read)
			continue
		}
		s.mcp.AddResource(resourceFrom(definition), reader.read)
	}
	return nil
}

// RegisterPromptBackend publishes client-guidance templates.
func (s *BaseServer) RegisterPromptBackend(ctx context.Context, backend contract.MCPPromptBackend) error {
	definitions, err := backend.ListPrompts(ctx, HostCaller())
	if err != nil {
		return fmt.Errorf("mcp prompt catalog: %w", err)
	}
	s.prompts = backend
	renderer := backendPrompt{governed: s.governed(), renderer: backend}
	for _, definition := range definitions {
		s.mcp.AddPrompt(promptFrom(definition), renderer.render)
	}
	return nil
}

func (s *BaseServer) visibleTools(ctx context.Context, tools []mcpgo.Tool) []mcpgo.Tool {
	return filterNamed(tools, s.allowedTools(ctx), func(tool mcpgo.Tool) string { return tool.Name })
}

func (s *BaseServer) visiblePrompts(ctx context.Context, prompts []mcpgo.Prompt) []mcpgo.Prompt {
	return filterNamed(prompts, s.allowedPrompts(ctx), func(prompt mcpgo.Prompt) string { return prompt.Name })
}

func (s *BaseServer) allowedTools(ctx context.Context) map[string]bool {
	if s.tools == nil {
		return nil
	}
	caller, err := s.governed().caller(ctx)
	if err != nil {
		return map[string]bool{}
	}
	definitions, err := s.tools.ListTools(ctx, caller)
	if err != nil {
		return map[string]bool{}
	}
	allowed := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		allowed[definition.Name] = Authorize(definition.Policy, caller) == nil
	}
	return allowed
}

// hiddenResources returns the backend entries the caller may not read; all of
// them when the caller or its catalog cannot be resolved.
func (s *BaseServer) hiddenResources(ctx context.Context) map[string]bool {
	hidden := make(map[string]bool, len(s.resourceKeys))
	for _, key := range s.resourceKeys {
		hidden[key] = true
	}
	if s.resources == nil {
		return hidden
	}
	caller, err := s.governed().caller(ctx)
	if err != nil {
		return hidden
	}
	definitions, err := s.resources.ListResources(ctx, caller)
	if err != nil {
		return hidden
	}
	for _, definition := range definitions {
		if requireScopes(definition.Policy.RequiredScopes, caller) == nil {
			delete(hidden, resourceKey(definition))
		}
	}
	return hidden
}

// resourceKey names a catalog entry the way the protocol lists it.
func resourceKey(definition domain.MCPResourceDefinition) string {
	if definition.URITemplate != "" {
		return definition.URITemplate
	}
	return definition.URI
}

func (s *BaseServer) allowedPrompts(ctx context.Context) map[string]bool {
	if s.prompts == nil {
		return nil
	}
	caller, err := s.governed().caller(ctx)
	if err != nil {
		return map[string]bool{}
	}
	definitions, err := s.prompts.ListPrompts(ctx, caller)
	if err != nil {
		return map[string]bool{}
	}
	allowed := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		allowed[definition.Name] = true
	}
	return allowed
}

// filterNamed keeps the entries a caller may see; a nil set means no
// caller-scoped catalog is registered and everything stays visible.
func filterNamed[T any](items []T, allowed map[string]bool, name func(T) string) []T {
	if allowed == nil {
		return items
	}
	visible := make([]T, 0, len(items))
	for _, item := range items {
		if allowed[name(item)] {
			visible = append(visible, item)
		}
	}
	return visible
}

func (s *BaseServer) MCPServer() *server.MCPServer { return s.mcp }

func (s *BaseServer) ServeStdio() error {
	return server.ServeStdio(s.mcp, server.WithStdioContextFunc(func(ctx context.Context) context.Context {
		return withTransport(ctx, domain.MCPTransportStdio)
	}))
}

func (s *BaseServer) ServeSSE(options ...server.SSEOption) *server.SSEServer {
	hook := server.WithSSEContextFunc(s.httpContext(domain.MCPTransportSSE))
	return server.NewSSEServer(s.mcp, append([]server.SSEOption{hook}, options...)...)
}

func (s *BaseServer) ServeStreamableHTTP() *server.StreamableHTTPServer {
	return server.NewStreamableHTTPServer(s.mcp, server.WithHTTPContextFunc(s.httpContext(domain.MCPTransportStreamableHTTP)))
}

func (s *BaseServer) httpContext(transport domain.MCPTransport) func(context.Context, *http.Request) context.Context {
	return func(ctx context.Context, request *http.Request) context.Context {
		return withTransport(s.ipHook(ctx, request), transport)
	}
}
