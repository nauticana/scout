package mcp

import (
	"context"
	"fmt"
	"net/http"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	keelhandler "github.com/nauticana/keel/handler"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
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
	mcp     *server.MCPServer
	ipHook  func(context.Context, *http.Request) context.Context
	source  string
	callers CallerResolver
	tools   contract.MCPToolCatalog
	prompts contract.MCPPromptCatalog
}

func NewServer(config ServerConfig) *BaseServer {
	base := &BaseServer{ipHook: config.ClientIPHook, source: config.Source, callers: config.Callers}
	if base.ipHook == nil {
		base.ipHook = keelhandler.WithClientIPContext
	}
	if base.callers == nil {
		base.callers = BaseCallerResolver{}
	}
	base.mcp = server.NewMCPServer(
		config.Name, config.Version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithPromptCapabilities(true),
		server.WithInstructions(config.Instructions),
		server.WithRecovery(),
		server.WithToolFilter(base.visibleTools),
		server.WithPromptFilter(base.visiblePrompts),
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
	s.tools = backend
	for _, definition := range definitions {
		s.Register(backendTool{
			governed:   s.governed(),
			definition: definition,
			tool:       toolFrom(definition),
			executor:   backend,
		})
	}
	return nil
}

// RegisterResourceBackend publishes catalog entries as fixed resources, or as
// URI templates when the entry carries one.
func (s *BaseServer) RegisterResourceBackend(ctx context.Context, backend contract.MCPResourceBackend) error {
	definitions, err := backend.ListResources(ctx, HostCaller())
	if err != nil {
		return fmt.Errorf("mcp resource catalog: %w", err)
	}
	reader := backendResource{governed: s.governed(), reader: backend}
	for _, definition := range definitions {
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
	caller, err := s.callers.Resolve(ctx)
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

func (s *BaseServer) allowedPrompts(ctx context.Context) map[string]bool {
	if s.prompts == nil {
		return nil
	}
	caller, err := s.callers.Resolve(ctx)
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
