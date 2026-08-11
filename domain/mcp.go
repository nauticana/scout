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

// EnvelopeMeta carries source, provenance, and pagination metadata.
type EnvelopeMeta struct {
	GeneratedAt string
	Source      string
	Provenance  *ProvenanceMeta
	Pagination  *PaginationMeta
}

// ProvenanceMeta describes data quality and attribution.
type ProvenanceMeta struct {
	VerificationLevel string
	CompletenessScore float64
	UpdatedAt         string
	VerifiedAt        string
	Sources           []SourceAttrib
	Attribution       string
}

// SourceAttrib identifies one upstream data source.
type SourceAttrib struct {
	Source     string
	ExternalID string
	ImportedAt string
}

// PaginationMeta describes one offset-based result window.
type PaginationMeta struct {
	Limit      int
	Offset     int
	Total      int
	HasMore    bool
	NextOffset int
}

// FieldDescriptor describes one discoverable domain field.
type FieldDescriptor struct {
	Name              string
	Kind              string
	Category          string
	Label             string
	Description       string
	ValueType         string
	AllowedValues     []string
	Example           string
	RelatedQuestionID string
	SourceOfTruth     string
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
