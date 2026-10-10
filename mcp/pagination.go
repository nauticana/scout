package mcp

import (
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/nauticana/scout/domain"
)

// PaginationSchema is the JSON Schema of the pagination result key, the
// mcp-v1 PaginationMeta.
var PaginationSchema = json.RawMessage(`{"type":"object","additionalProperties":false,` +
	`"properties":{"limit":{"type":"integer"},"offset":{"type":"integer"},"total":{"type":"integer"},` +
	`"has_more":{"type":"boolean"},"next_offset":{"type":"integer"}},"required":["limit","offset","total","has_more"]}`)

// PaginationInstructions explains the pagination key once, for a server's instructions.
const PaginationInstructions = "A tool that takes offset returns pagination: limit, offset, total and has_more; " +
	"while has_more is true, pass next_offset as offset to read the next page."

const paginationKey = "pagination"

// pages reports whether a tool takes the paging inputs: an offset property.
func pages(input json.RawMessage) bool {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(input, &schema) != nil {
		return false
	}
	_, ok := schema.Properties["offset"]
	return ok
}

// shapeDefinition adds the pagination key to the output schema of a paging
// tool that declares one and does not name the key itself.
func shapeDefinition(definition domain.MCPToolDefinition) (domain.MCPToolDefinition, error) {
	if len(definition.OutputSchema) == 0 || !pages(definition.InputSchema) {
		return definition, nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(definition.OutputSchema, &schema); err != nil {
		return definition, fmt.Errorf("%w: mcp tool %q output schema: %w", domain.ErrValidation, definition.Name, err)
	}
	properties := map[string]json.RawMessage{}
	if raw, ok := schema["properties"]; ok {
		if err := json.Unmarshal(raw, &properties); err != nil {
			return definition, fmt.Errorf("%w: mcp tool %q output properties: %w", domain.ErrValidation, definition.Name, err)
		}
	}
	if _, declared := properties[paginationKey]; declared {
		return definition, nil
	}
	properties[paginationKey] = PaginationSchema
	encoded, err := json.Marshal(properties)
	if err != nil {
		return definition, err
	}
	schema["properties"] = encoded
	if definition.OutputSchema, err = json.Marshal(schema); err != nil {
		return definition, err
	}
	return definition, nil
}

// ListedTool is the tools/list entry Scout publishes for a backend definition.
func ListedTool(definition domain.MCPToolDefinition) (mcpgo.Tool, error) {
	shaped, err := shapeDefinition(definition)
	if err != nil {
		return mcpgo.Tool{}, err
	}
	return toolFrom(shaped), nil
}

// withPagination adds a paging tool's pagination to its encoded structured
// content when the data does not carry the key itself.
func withPagination(encoded []byte, meta *domain.EnvelopeMeta) ([]byte, error) {
	if meta == nil || meta.Pagination == nil {
		return encoded, nil
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &data); err != nil || data == nil {
		return encoded, nil
	}
	if _, present := data[paginationKey]; present {
		return encoded, nil
	}
	pagination, err := json.Marshal(wirePagination(meta.Pagination))
	if err != nil {
		return nil, err
	}
	data[paginationKey] = pagination
	return json.Marshal(data)
}
