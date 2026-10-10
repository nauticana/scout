package mcp

import (
	"context"
	"fmt"
	"maps"
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
	// OnDenied observes each backend tool call refused before its executor runs:
	// an unresolved caller, a tool outside the caller's catalog, or missing scopes.
	OnDenied DeniedFunc
	// OmitOutputSchemas lists backend tools without outputSchema. Results are
	// still validated against it and returned as structured content.
	OmitOutputSchemas bool
}

// DeniedFunc receives a refused call's caller, tool name, and refusal.
type DeniedFunc func(ctx context.Context, caller domain.MCPCaller, tool string, err error)

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
	toolNames    map[string]bool
	onDenied     DeniedFunc
	omitOutputs  bool
	listeners    *toolListeners
}

func NewServer(config ServerConfig) *BaseServer {
	base := &BaseServer{ipHook: config.ClientIPHook, source: config.Source, callers: config.Callers, onDenied: config.OnDenied,
		omitOutputs: config.OmitOutputSchemas, listeners: newToolListeners()}
	if base.ipHook == nil {
		base.ipHook = keelhandler.WithClientIPContext
	}
	if base.callers == nil {
		base.callers = BaseCallerResolver{}
	}
	// Listings are narrowed after the fact; a tool call is checked once, in its handler.
	hooks := &server.Hooks{}
	hooks.AddAfterListTools(func(ctx context.Context, _ any, _ *mcpgo.ListToolsRequest, result *mcpgo.ListToolsResult) {
		hidden := base.hiddenTools(ctx)
		result.Tools = slices.DeleteFunc(result.Tools, func(tool mcpgo.Tool) bool { return hidden[tool.Name] })
	})
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
	base.trackListeners(hooks)
	base.mcp = server.NewMCPServer(
		config.Name, config.Version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithPromptCapabilities(true),
		server.WithInstructions(config.Instructions),
		server.WithRecovery(),
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

// RegisterToolBackend publishes the backend's Catalog and routes every call
// through the backend after scope authorization. Callers list only the subset
// ListTools returns for them.
func (s *BaseServer) RegisterToolBackend(ctx context.Context, backend contract.MCPToolBackend) error {
	definitions, err := backend.Catalog(ctx)
	if err != nil {
		return fmt.Errorf("mcp tool catalog: %w", err)
	}
	definitions = slices.Clone(definitions)
	outputs := make([]*jsonschema.Schema, len(definitions))
	for i := range definitions {
		if definitions[i], err = shapeDefinition(definitions[i]); err != nil {
			return err
		}
		if outputs[i], err = outputSchema(definitions[i]); err != nil {
			return err
		}
	}
	s.tools = backend
	s.toolNames = make(map[string]bool, len(definitions))
	for i, definition := range definitions {
		s.toolNames[definition.Name] = true
		tool := toolFrom(definition)
		if s.omitOutputs {
			tool.RawOutputSchema = nil
		}
		s.Register(backendTool{
			governed:   s.governed(),
			definition: definition,
			tool:       tool,
			backend:    backend,
			output:     outputs[i],
			paged:      pages(definition.InputSchema),
			onDenied:   s.onDenied,
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

func (s *BaseServer) visiblePrompts(ctx context.Context, prompts []mcpgo.Prompt) []mcpgo.Prompt {
	return filterNamed(prompts, s.allowedPrompts(ctx), func(prompt mcpgo.Prompt) string { return prompt.Name })
}

// hiddenTools returns the backend tools the caller may not call; all of them
// when the caller or its catalog cannot be resolved. Directly registered tools
// are not caller-scoped.
func (s *BaseServer) hiddenTools(ctx context.Context) map[string]bool {
	hidden := maps.Clone(s.toolNames)
	if s.tools == nil {
		return hidden
	}
	caller, err := s.governed().caller(ctx)
	if err != nil {
		return hidden
	}
	definitions, err := s.tools.ListTools(ctx, caller)
	if err != nil {
		return hidden
	}
	for _, definition := range definitions {
		if Authorize(definition.Policy, caller) == nil {
			delete(hidden, definition.Name)
		}
	}
	return hidden
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
	return server.NewSSEServer(s.mcp, append(slices.Clone(options), hook)...)
}

func (s *BaseServer) ServeStreamableHTTP(options ...server.StreamableHTTPOption) *server.StreamableHTTPServer {
	hook := server.WithHTTPContextFunc(s.httpContext(domain.MCPTransportStreamableHTTP))
	return server.NewStreamableHTTPServer(s.mcp, append(slices.Clone(options), hook)...)
}

func (s *BaseServer) httpContext(transport domain.MCPTransport) func(context.Context, *http.Request) context.Context {
	return func(ctx context.Context, request *http.Request) context.Context {
		return withTransport(s.ipHook(ctx, request), transport)
	}
}
