package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/nauticana/scout/domain"
)

func TestToolProjectionCarriesRawSchema(t *testing.T) {
	tool := toolFrom(domain.MCPToolDefinition{
		Name:        "search",
		Description: "search things",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		Annotations: domain.MCPToolAnnotations{Title: "Search"},
	})
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}
	if !strings.Contains(string(encoded), `"q"`) || !strings.Contains(string(encoded), `"Search"`) {
		t.Fatalf("tool = %s", encoded)
	}
	bare := toolFrom(domain.MCPToolDefinition{Name: "ping"})
	if bare.InputSchema.Type != "object" {
		t.Fatalf("bare tool schema = %+v", bare.InputSchema)
	}
}

func TestContentsProjection(t *testing.T) {
	projected := contentsFrom([]domain.MCPResourceContent{
		{URI: "scout://text", MIMEType: "text/plain", Text: "hello"},
		{URI: "scout://blob", MIMEType: "application/octet-stream", Blob: []byte{1, 2, 3}},
	})
	if _, ok := projected[0].(mcpgo.TextResourceContents); !ok {
		t.Fatalf("text content = %T", projected[0])
	}
	blob, ok := projected[1].(mcpgo.BlobResourceContents)
	if !ok || blob.Blob != "AQID" {
		t.Fatalf("blob content = %#v", projected[1])
	}
}

func TestProjectionsCarryTitlesInTheirOwnField(t *testing.T) {
	resource := resourceFrom(domain.MCPResourceDefinition{URI: "scout://r", Name: "r", Title: "Report", Description: "the report", MIMEType: "text/plain"})
	template := resourceTemplateFrom(domain.MCPResourceDefinition{URITemplate: "scout://r/{id}", Name: "r", Title: "Report", Description: "a report"})
	if resource.Title != "Report" || resource.Description != "the report" || template.Title != "Report" || template.Description != "a report" {
		t.Fatalf("resource = %+v, template = %+v", resource, template)
	}
	prompt := promptFrom(domain.MCPPromptDefinition{Name: "audit", Title: "Audit", Description: "audit a page",
		Arguments: []domain.MCPPromptArgument{{Name: "url", Title: "Page URL", Description: "the page", Required: true}}})
	if prompt.Title != "Audit" || prompt.Description != "audit a page" || len(prompt.Arguments) != 1 ||
		prompt.Arguments[0].Title != "Page URL" || !prompt.Arguments[0].Required {
		t.Fatalf("prompt = %+v", prompt)
	}
	untitled := resourceFrom(domain.MCPResourceDefinition{URI: "scout://u", Name: "u"})
	if encoded, _ := json.Marshal(untitled); strings.Contains(string(encoded), `"title"`) {
		t.Fatalf("an untitled resource must omit the title: %s", encoded)
	}
	rendered := promptResultFrom(domain.MCPPromptResult{Description: "d", Messages: []domain.MCPPromptMessage{{Role: domain.MCPPromptRoleUser, Text: "hi"}}})
	if rendered.Description != "d" || len(rendered.Messages) != 1 || rendered.Messages[0].Role != mcpgo.RoleUser {
		t.Fatalf("prompt result = %+v", rendered)
	}
	linked := NewEnvelopes("test").Result(domain.MCPToolResult{Evidence: []domain.MCPResourceLink{{URI: "scout://e", Name: "e", Title: "Evidence"}}})
	if encoded, _ := json.Marshal(linked); !strings.Contains(string(encoded), `"title":"Evidence"`) {
		t.Fatalf("resource link = %s", encoded)
	}
}
