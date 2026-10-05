package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/nauticana/scout/domain"
)

// schemaBackend publishes one tool with an output schema and returns data.
type schemaBackend struct{ data any }

func (backend schemaBackend) ListTools(context.Context, domain.MCPCaller) ([]domain.MCPToolDefinition, error) {
	return []domain.MCPToolDefinition{{
		Name: "count", Description: "count things",
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
	}}, nil
}

func (backend schemaBackend) ExecuteTool(context.Context, domain.MCPToolCall) (domain.MCPToolResult, error) {
	return domain.MCPToolResult{Data: backend.data}, nil
}

func callCount(t *testing.T, data any) string {
	t.Helper()
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", Source: "test"})
	if err := srv.RegisterToolBackend(context.Background(), schemaBackend{data: data}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return call(t, srv, remoteContext("read"), "tools/call", map[string]any{"name": "count"})
}

func TestOutputSchemaToolReturnsStructuredContent(t *testing.T) {
	result := callCount(t, map[string]int{"n": 3})
	if !strings.Contains(result, `"structuredContent":{"n":3}`) || !strings.Contains(result, `\"source\": \"test\"`) {
		t.Fatalf("result = %s", result)
	}
}

func TestRegisterToolBackendRejectsInvalidOutputSchema(t *testing.T) {
	for name, schema := range map[string]json.RawMessage{
		"malformed":   json.RawMessage(`{"type":"object","properties":`),
		"unsupported": json.RawMessage(`{"type":"object","allOf":[]}`),
		"not object":  json.RawMessage(`{"type":"array"}`),
	} {
		t.Run(name, func(t *testing.T) {
			backend := schemaBackend{}
			definition, _ := backend.ListTools(context.Background(), domain.MCPCaller{})
			definition[0].OutputSchema = schema
			catalog := &definedSchemaBackend{definition: definition[0]}
			srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0"})
			if err := srv.RegisterToolBackend(context.Background(), catalog); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("registration error = %v, want validation", err)
			}
		})
	}
}

type definedSchemaBackend struct {
	definition domain.MCPToolDefinition
	data       any
}

func (backend *definedSchemaBackend) ListTools(context.Context, domain.MCPCaller) ([]domain.MCPToolDefinition, error) {
	return []domain.MCPToolDefinition{backend.definition}, nil
}

func (backend *definedSchemaBackend) ExecuteTool(context.Context, domain.MCPToolCall) (domain.MCPToolResult, error) {
	return domain.MCPToolResult{Data: backend.data}, nil
}

func TestOutputSchemaToolRefusesNonConformingData(t *testing.T) {
	for name, data := range map[string]any{
		"wrong shape": map[string]string{"n": "three"},
		"no data":     nil,
		"not JSON":    make(chan int),
	} {
		result := callCount(t, data)
		if !strings.Contains(result, `"isError":true`) || strings.Contains(result, "structuredContent") {
			t.Errorf("%s: result = %s", name, result)
		}
	}
}

// A schema mcp-go's own validator would skip is still enforced.
func TestOutputSchemaIsEnforcedByTheRegisteredSchema(t *testing.T) {
	for _, schema := range []string{
		`{"$schema":"https://example.invalid/draft","type":"object","required":["n"]}`,
		`{"type":"object","required":["n","n"]}`,
	} {
		srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", Source: "test"})
		backend := &definedSchemaBackend{
			definition: domain.MCPToolDefinition{Name: "count", Description: "count things", OutputSchema: json.RawMessage(schema)},
			data:       map[string]int{"x": 1},
		}
		if err := srv.RegisterToolBackend(context.Background(), backend); err != nil {
			t.Fatalf("register: %v", err)
		}
		result := call(t, srv, remoteContext("read"), "tools/call", map[string]any{"name": "count"})
		if !strings.Contains(result, `"isError":true`) || strings.Contains(result, "structuredContent") {
			t.Errorf("%s: result = %s", schema, result)
		}
	}
}

func TestToolWithoutOutputSchemaReturnsTextOnly(t *testing.T) {
	server, _ := registeredServer(t)
	result := call(t, server, remoteContext("read"), "tools/call", map[string]any{"name": "search"})
	if strings.Contains(result, "structuredContent") || strings.Contains(result, `"isError":true`) {
		t.Fatalf("result = %s", result)
	}
}

func TestToolErrorReturnsStructuredContent(t *testing.T) {
	resetAt := "2026-10-05T00:00:00Z"
	backend := &definedSchemaBackend{definition: domain.MCPToolDefinition{Name: "count", Description: "count things",
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)}}
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0"})
	if err := srv.RegisterToolBackend(context.Background(), &toolErrorBackend{definedSchemaBackend: backend, result: domain.MCPToolResult{
		Error: &domain.MCPToolError{Code: "quota_exhausted", Message: "monthly quota used", Details: map[string]any{"reset_at": resetAt}},
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	result := call(t, srv, remoteContext("read"), "tools/call", map[string]any{"name": "count"})
	want := `{"code":"quota_exhausted","message":"monthly quota used","details":{"reset_at":"2026-10-05T00:00:00Z"}}`
	if !strings.Contains(result, `"structuredContent":`+want) || !strings.Contains(result, `"isError":true`) || !strings.Contains(result, `quota_exhausted\",\"message`) {
		t.Fatalf("result = %s", result)
	}
}

func TestToolErrorRefusesMalformedErrors(t *testing.T) {
	for name, result := range map[string]domain.MCPToolResult{
		"no code":          {Error: &domain.MCPToolError{Message: "failed"}},
		"no message":       {Error: &domain.MCPToolError{Code: "failed"}},
		"unencodable":      {Error: &domain.MCPToolError{Code: "failed", Message: "failed", Details: map[string]any{"c": make(chan int)}}},
		"with data":        {Error: &domain.MCPToolError{Code: "failed", Message: "failed"}, Data: map[string]int{"n": 1}},
		"with elicitation": {Error: &domain.MCPToolError{Code: "failed", Message: "failed"}, Elicit: map[string]domain.MCPElicitation{"q": {Message: "?"}}},
	} {
		t.Run(name, func(t *testing.T) {
			tool := backendTool{governed: governed{callers: BaseCallerResolver{}}, definition: domain.MCPToolDefinition{Name: "count"},
				executor: &toolErrorBackend{definedSchemaBackend: &definedSchemaBackend{}, result: result}}
			out, err := tool.Handle(remoteContext("read"), mcpgo.CallToolRequest{})
			if err != nil || !out.IsError || out.StructuredContent != nil || !strings.Contains(out.Content[0].(mcpgo.TextContent).Text, domain.ErrContractFailed.Error()) {
				t.Fatalf("result = %+v, err = %v", out, err)
			}
		})
	}
}

type toolErrorBackend struct {
	*definedSchemaBackend
	result domain.MCPToolResult
}

func (backend *toolErrorBackend) ExecuteTool(context.Context, domain.MCPToolCall) (domain.MCPToolResult, error) {
	return backend.result, nil
}
