package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

func schemaServer(t *testing.T, omit bool) *BaseServer {
	t.Helper()
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", OmitOutputSchemas: omit})
	if err := srv.RegisterToolBackend(context.Background(), schemaBackend{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return srv
}

// The tool answers with the schemas tools/list would carry without the omission.
func TestToolSchemaAnswersWhatTheListingOmits(t *testing.T) {
	srv := schemaServer(t, true)
	if listed := call(t, srv, remoteContext("read"), "tools/list", map[string]any{}); !strings.Contains(listed, `"`+ToolSchemaToolName+`"`) {
		t.Fatalf("listing = %s", listed)
	}
	result := call(t, srv, remoteContext("read"), "tools/call", map[string]any{"name": ToolSchemaToolName, "arguments": map[string]any{"name": "count"}})
	for _, want := range []string{`"structuredContent":{"input_schema":{`, `"output_schema":{"type":"object","properties":{"n":{"type":"integer"}}`, `"name":"count"`} {
		if !strings.Contains(result, want) {
			t.Fatalf("result lacks %s: %s", want, result)
		}
	}
	if listed := call(t, schemaServer(t, false), remoteContext("read"), "tools/list", map[string]any{}); strings.Contains(listed, ToolSchemaToolName) {
		t.Fatalf("a server that lists output schemas registers no schema tool: %s", listed)
	}
}

// An unknown tool, one outside the caller's catalog, and one whose scopes the
// caller lacks all read as not found.
func TestToolSchemaHidesWhatTheCallerCannotCall(t *testing.T) {
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", OmitOutputSchemas: true})
	if err := srv.RegisterToolBackend(context.Background(), &toolBackendFake{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	schemaOf := func(scopes, name string) string {
		return call(t, srv, remoteContext(scopes), "tools/call", map[string]any{"name": ToolSchemaToolName, "arguments": map[string]any{"name": name}})
	}
	if result := schemaOf("write", "submit"); !strings.Contains(result, `"output_schema":{}`) {
		t.Fatalf("a tool without an output schema answers an empty one: %s", result)
	}
	for _, name := range []string{"unknown", "submit"} {
		if result := schemaOf("read", name); !strings.Contains(result, "not found") || strings.Contains(result, "structuredContent") {
			t.Fatalf("%s: %s", name, result)
		}
	}
	if result := call(t, srv, remoteContext("read"), "tools/call", map[string]any{"name": ToolSchemaToolName}); !strings.Contains(result, "name is required") {
		t.Fatalf("missing name: %s", result)
	}
}

func TestToolSchemaRefusesABackendToolOfTheSameName(t *testing.T) {
	backend := &definedSchemaBackend{definition: domain.MCPToolDefinition{Name: ToolSchemaToolName, Description: "own"}}
	srv := NewServer(ServerConfig{Name: "test", Version: "1.0.0", OmitOutputSchemas: true})
	if err := srv.RegisterToolBackend(context.Background(), backend); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("register = %v, want conflict", err)
	}
}
