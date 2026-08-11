package mcp

import (
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

// Envelopes builds data and metadata tool results for one server source.
type Envelopes struct {
	Source string
	now    func() time.Time
}

func NewEnvelopes(source string) Envelopes { return Envelopes{Source: source} }

func (e Envelopes) timestamp() string {
	if e.now != nil {
		return e.now().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

func (e Envelopes) Wrap(data any, meta *domain.EnvelopeMeta) *mcpgo.CallToolResult {
	wire := wireMeta(meta)
	if wire == nil {
		wire = &api.EnvelopeMeta{}
	}
	if wire.GeneratedAt == "" {
		wire.GeneratedAt = e.timestamp()
	}
	if wire.Source == "" {
		wire.Source = e.Source
	}
	encoded, err := json.MarshalIndent(api.Envelope{Data: data, Meta: wire}, "", "  ")
	if err != nil {
		return mcpgo.NewToolResultError("failed to marshal envelope: " + err.Error())
	}
	return mcpgo.NewToolResultText(string(encoded))
}

func (e Envelopes) WrapWithProvenance(data any, provenance *domain.ProvenanceMeta) *mcpgo.CallToolResult {
	return e.Wrap(data, &domain.EnvelopeMeta{Provenance: provenance})
}

func (e Envelopes) WrapWithPagination(data any, limit, offset, total int, hasMore bool) *mcpgo.CallToolResult {
	pagination := &domain.PaginationMeta{Limit: limit, Offset: offset, Total: total, HasMore: hasMore}
	if hasMore {
		pagination.NextOffset = offset + limit
	}
	return e.Wrap(data, &domain.EnvelopeMeta{Pagination: pagination})
}

// Result projects a backend tool result: the envelope carries the data, and
// evidence and durable task references become resource links.
func (e Envelopes) Result(result domain.MCPToolResult) *mcpgo.CallToolResult {
	wrapped := e.Wrap(result.Data, result.Meta)
	for _, link := range result.Evidence {
		wrapped.Content = append(wrapped.Content, mcpgo.NewResourceLink(link.URI, link.Name, link.Description, link.MIMEType))
	}
	if task := result.Task; task != nil && task.ResourceURI != "" {
		wrapped.Content = append(wrapped.Content, mcpgo.NewResourceLink(task.ResourceURI, task.ID, "task status: "+task.Status, "application/json"))
	}
	return wrapped
}

func WrapError(err error) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultErrorFromErr("tool execution failed", err)
}
