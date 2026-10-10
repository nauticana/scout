package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

// pagedBackend publishes one tool taking offset and returns a page window.
type pagedBackend struct {
	output json.RawMessage
	result domain.MCPToolResult
}

func (backend pagedBackend) Catalog(context.Context) ([]domain.MCPToolDefinition, error) {
	return []domain.MCPToolDefinition{{
		Name: "list_rows", Description: "list rows",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"},"offset":{"type":"integer"}}}`),
		OutputSchema: backend.output,
	}}, nil
}

func (backend pagedBackend) ListTools(ctx context.Context, _ domain.MCPCaller) ([]domain.MCPToolDefinition, error) {
	return backend.Catalog(ctx)
}

func (backend pagedBackend) ExecuteTool(context.Context, domain.MCPToolCall) (domain.MCPToolResult, error) {
	return backend.result, nil
}

var closedRows = json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"rows":{"type":"array"}},"required":["rows"]}`)

func TestPagingToolCarriesPaginationInStructuredContent(t *testing.T) {
	page := &domain.EnvelopeMeta{Pagination: &domain.PaginationMeta{Limit: 2, Offset: 0, Total: 5, HasMore: true, NextOffset: 2}}
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0"})
	backend := pagedBackend{output: closedRows, result: domain.MCPToolResult{Data: map[string]any{"rows": []int{1, 2}}, Meta: page}}
	if err := srv.RegisterToolBackend(context.Background(), backend); err != nil {
		t.Fatal(err)
	}
	listed := call(t, srv, remoteContext("read"), "tools/list", map[string]any{})
	if !strings.Contains(listed, `"pagination":{"type":"object","additionalProperties":false`) {
		t.Fatalf("listing lacks the pagination key: %s", listed)
	}
	result := call(t, srv, remoteContext("read"), "tools/call", map[string]any{"name": "list_rows"})
	want := `"structuredContent":{"pagination":{"limit":2,"offset":0,"total":5,"has_more":true,"next_offset":2},"rows":[1,2]}`
	if !strings.Contains(result, want) {
		t.Fatalf("result = %s", result)
	}
	backend.result.Meta = nil
	srv = NewServer(ServerConfig{Name: "test", Version: "1.0.0"})
	if err := srv.RegisterToolBackend(context.Background(), backend); err != nil {
		t.Fatal(err)
	}
	if result = call(t, srv, remoteContext("read"), "tools/call", map[string]any{"name": "list_rows"}); !strings.Contains(result, `"structuredContent":{"rows":[1,2]}`) {
		t.Fatalf("result without a page = %s", result)
	}
}

func TestShapeDefinitionLeavesOtherToolsAlone(t *testing.T) {
	paging := json.RawMessage(`{"type":"object","properties":{"offset":{"type":"integer"}}}`)
	declared := json.RawMessage(`{"type":"object","properties":{"pagination":{"type":"string"}}}`)
	for name, definition := range map[string]domain.MCPToolDefinition{
		"no paging input":     {Name: "a", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: closedRows},
		"no output schema":    {Name: "b", InputSchema: paging},
		"declares pagination": {Name: "c", InputSchema: paging, OutputSchema: declared},
	} {
		shaped, err := shapeDefinition(definition)
		if err != nil || string(shaped.OutputSchema) != string(definition.OutputSchema) {
			t.Errorf("%s: shaped %s, err %v", name, shaped.OutputSchema, err)
		}
	}
	if _, err := shapeDefinition(domain.MCPToolDefinition{Name: "d", InputSchema: paging, OutputSchema: json.RawMessage(`{"properties":[]}`)}); err == nil {
		t.Fatal("malformed output properties were accepted")
	}
}
