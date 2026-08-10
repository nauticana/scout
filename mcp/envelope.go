package mcp

import (
	"encoding/json"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

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
	if meta == nil {
		meta = &domain.EnvelopeMeta{}
	}
	if meta.GeneratedAt == "" {
		meta.GeneratedAt = e.timestamp()
	}
	if meta.Source == "" {
		meta.Source = e.Source
	}
	encoded, err := json.MarshalIndent(domain.Envelope{Data: data, Meta: meta}, "", "  ")
	if err != nil {
		return mcpgo.NewToolResultError("failed to marshal envelope: " + err.Error())
	}
	return mcpgo.NewToolResultText(string(encoded))
}

func (e Envelopes) WrapWithProvenance(data any, provenance *domain.ProvenanceMeta) *mcpgo.CallToolResult {
	meta := &domain.EnvelopeMeta{Provenance: provenance}
	return e.Wrap(data, meta)
}

func (e Envelopes) WrapWithPagination(data any, limit, offset, total int, hasMore bool) *mcpgo.CallToolResult {
	pagination := &domain.PaginationMeta{Limit: limit, Offset: offset, Total: total, HasMore: hasMore}
	if hasMore {
		pagination.NextOffset = offset + limit
	}
	return e.Wrap(data, &domain.EnvelopeMeta{Pagination: pagination})
}

func WrapError(err error) *mcpgo.CallToolResult {
	return mcpgo.NewToolResultErrorFromErr("tool execution failed", err)
}
