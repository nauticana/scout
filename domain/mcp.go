package domain

import (
	"encoding/json"
)

// MCPTransport identifies the Keel transport that admitted a call.
type MCPTransport string

const (
	MCPTransportStdio          MCPTransport = "stdio"
	MCPTransportSSE            MCPTransport = "sse"
	MCPTransportStreamableHTTP MCPTransport = "streamable_http"
)

// MCPCaller contains server-derived identity and request context for one call.
type MCPCaller struct {
	TenantID      int64
	ActorID       int64
	CredentialID  int64
	Subject       string
	Scopes        []string
	SessionID     string
	ClientIP      string
	Transport     MCPTransport
	Authenticated bool
	HostTrusted   bool
}

// MCPServerDefinition contains product-owned values mapped to Keel server configuration.
type MCPServerDefinition struct {
	Name         string
	Version      string
	Instructions string
	Source       string
}

// MCPToolAnnotations contains standard client-facing MCP safety hints.
type MCPToolAnnotations struct {
	Title           string
	ReadOnlyHint    *bool
	DestructiveHint *bool
	IdempotentHint  *bool
	OpenWorldHint   *bool
}

// MCPToolPolicy declares server-enforced access, quota, approval, and audit requirements.
type MCPToolPolicy struct {
	RequiredScopes   []string
	QuotaResource    string
	QuotaAmount      int64
	ApprovalRequired bool
	AuditCategory    string
}

// MCPToolDefinition is an SDK-neutral MCP tool manifest entry.
type MCPToolDefinition struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Annotations  MCPToolAnnotations
	Policy       MCPToolPolicy
}

// MCPToolCall carries validated arguments and server-derived caller context.
type MCPToolCall struct {
	Caller    MCPCaller
	RequestID string
	Name      string
	Arguments map[string]any
}

// MCPResourceLink points a result consumer to supporting MCP content.
type MCPResourceLink struct {
	URI         string
	Name        string
	Title       string
	Description string
	MIMEType    string
}

// MCPTaskReference identifies durable work started by a bounded MCP call.
type MCPTaskReference struct {
	ID          string
	Status      string
	ResourceURI string
}

// MCPToolResult is projected into Keel's text envelope and optional resource links.
type MCPToolResult struct {
	Data     any
	Meta     *EnvelopeMeta
	Evidence []MCPResourceLink
	Task     *MCPTaskReference
}

// Envelope wraps an MCP tool result with optional metadata.
type Envelope struct {
	Data any           `json:"data"`
	Meta *EnvelopeMeta `json:"_meta,omitempty"`
}

// EnvelopeMeta carries source, provenance, and pagination metadata.
type EnvelopeMeta struct {
	GeneratedAt string          `json:"generated_at"`
	Source      string          `json:"source,omitempty"`
	Provenance  *ProvenanceMeta `json:"provenance,omitempty"`
	Pagination  *PaginationMeta `json:"pagination,omitempty"`
}

// ProvenanceMeta describes data quality and attribution.
type ProvenanceMeta struct {
	VerificationLevel string         `json:"verification_level,omitempty"`
	CompletenessScore float64        `json:"completeness_score,omitempty"`
	UpdatedAt         string         `json:"updated_at,omitempty"`
	VerifiedAt        string         `json:"verified_at,omitempty"`
	Sources           []SourceAttrib `json:"sources,omitempty"`
	Attribution       string         `json:"attribution,omitempty"`
}

// SourceAttrib identifies one upstream data source.
type SourceAttrib struct {
	Source     string `json:"source"`
	ExternalID string `json:"external_id,omitempty"`
	ImportedAt string `json:"imported_at,omitempty"`
}

// PaginationMeta describes one offset-based result window.
type PaginationMeta struct {
	Limit      int  `json:"limit"`
	Offset     int  `json:"offset"`
	Total      int  `json:"total"`
	HasMore    bool `json:"has_more"`
	NextOffset int  `json:"next_offset,omitempty"`
}

// FieldDescriptor describes one discoverable domain field.
type FieldDescriptor struct {
	Name              string   `json:"name"`
	Kind              string   `json:"kind"`
	Category          string   `json:"category,omitempty"`
	Label             string   `json:"label,omitempty"`
	Description       string   `json:"description,omitempty"`
	ValueType         string   `json:"value_type,omitempty"`
	AllowedValues     []string `json:"allowed_values,omitempty"`
	Example           string   `json:"example,omitempty"`
	RelatedQuestionID string   `json:"related_question_id,omitempty"`
	SourceOfTruth     string   `json:"source_of_truth,omitempty"`
}

// MCPResourceDefinition is an SDK-neutral MCP resource manifest entry.
type MCPResourceDefinition struct {
	URI         string
	URITemplate string
	Name        string
	Title       string
	Description string
	MIMEType    string
}

// MCPResourceRequest carries one URI and its server-derived caller context.
type MCPResourceRequest struct {
	Caller    MCPCaller
	RequestID string
	URI       string
}

// MCPResourceContent contains one text or binary resource payload.
type MCPResourceContent struct {
	URI      string
	MIMEType string
	Text     string
	Blob     []byte
}

// MCPPromptArgument defines one client-supplied prompt template argument.
type MCPPromptArgument struct {
	Name        string
	Description string
	Required    bool
}

// MCPPromptDefinition is an SDK-neutral MCP prompt manifest entry.
type MCPPromptDefinition struct {
	Name        string
	Title       string
	Description string
	Arguments   []MCPPromptArgument
}

// MCPPromptRole identifies the author of a rendered prompt message.
type MCPPromptRole string

const (
	MCPPromptRoleUser      MCPPromptRole = "user"
	MCPPromptRoleAssistant MCPPromptRole = "assistant"
)

// MCPPromptRequest carries prompt arguments and server-derived caller context.
type MCPPromptRequest struct {
	Caller    MCPCaller
	RequestID string
	Name      string
	Arguments map[string]string
}

// MCPPromptMessage is one text message in a rendered prompt.
type MCPPromptMessage struct {
	Role MCPPromptRole
	Text string
}

// MCPPromptResult contains client-guidance messages without executing tools.
type MCPPromptResult struct {
	Description string
	Messages    []MCPPromptMessage
}
