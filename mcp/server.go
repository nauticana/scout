package mcp

import (
	"context"
	"net/http"

	"github.com/mark3labs/mcp-go/server"
	keelhandler "github.com/nauticana/keel/handler"
)

// ServerConfig contains product-owned MCP server identity and copy.
type ServerConfig struct {
	Name         string
	Version      string
	Instructions string
	Source       string
	ClientIPHook func(context.Context, *http.Request) context.Context
}

// BaseServer wraps mcp-go with name-keyed provider registration.
type BaseServer struct {
	mcp    *server.MCPServer
	ipHook func(context.Context, *http.Request) context.Context
	source string
}

func NewServer(config ServerConfig) *BaseServer {
	hook := config.ClientIPHook
	if hook == nil {
		hook = keelhandler.WithClientIPContext
	}
	return &BaseServer{
		mcp: server.NewMCPServer(
			config.Name, config.Version,
			server.WithToolCapabilities(true),
			server.WithResourceCapabilities(true, true),
			server.WithInstructions(config.Instructions),
		),
		ipHook: hook,
		source: config.Source,
	}
}

func (s *BaseServer) Envelopes() Envelopes { return NewEnvelopes(s.source) }

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

func (s *BaseServer) MCPServer() *server.MCPServer { return s.mcp }

func (s *BaseServer) ServeStdio() error { return server.ServeStdio(s.mcp) }

func (s *BaseServer) ServeSSE(options ...server.SSEOption) *server.SSEServer {
	return server.NewSSEServer(s.mcp, append([]server.SSEOption{server.WithSSEContextFunc(s.ipHook)}, options...)...)
}

func (s *BaseServer) ServeStreamableHTTP() *server.StreamableHTTPServer {
	return server.NewStreamableHTTPServer(s.mcp, server.WithHTTPContextFunc(s.ipHook))
}
