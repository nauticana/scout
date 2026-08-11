package api

// The mcp-v1 compatibility profile. MCP frames and manifest entries belong to
// the protocol library; these are the Scout-owned payloads carried inside a
// tool result, so their field names are a published contract.

// Envelope is the mcp-v1 tool result payload.
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

// FieldDescriptor is one discoverable field returned by field discovery.
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
