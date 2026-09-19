package domain

// EvidenceSource says where a piece of evidence came from, which decides how it is verified.
type EvidenceSource string

const (
	// EvidenceObject is an immutable object; Object.Digest must verify.
	EvidenceObject EvidenceSource = "object"
	// EvidenceToolResult is the validated output of a governed tool call in this turn.
	EvidenceToolResult EvidenceSource = "tool_result"
	// EvidenceMCPResource is a resource link an MCP tool result returned.
	EvidenceMCPResource EvidenceSource = "mcp_resource"
)

// EvidenceRef points at one piece of support for a claim.
type EvidenceRef struct {
	ID     string         `json:"id"`
	Source EvidenceSource `json:"source"`
	Object ObjectRef      `json:"object,omitzero"`
	CallID string         `json:"call_id,omitempty"`
	URI    string         `json:"uri,omitempty"`
}

// Claim is one factual statement and the evidence ids offered for it.
type Claim struct {
	Text        string   `json:"text"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// EvidencedAnswer is the optional result shape for agents whose output makes
// factual claims; agents that make none keep returning plain output.
type EvidencedAnswer struct {
	Answer   string        `json:"answer"`
	Claims   []Claim       `json:"claims"`
	Evidence []EvidenceRef `json:"evidence"`
}

// ClaimFinding explains why one claim is unsupported.
type ClaimFinding struct {
	ClaimIndex int    `json:"claim_index"`
	Claim      string `json:"claim"`
	Reason     string `json:"reason"`
}

// EvidenceReport separates the claims that verified from those that did not, so
// a caller can keep the safe part of an answer.
type EvidenceReport struct {
	Supported   []Claim        `json:"supported"`
	Unsupported []ClaimFinding `json:"unsupported"`
}

// ToolEvidence is one governed tool call of the turn that returned validated output.
type ToolEvidence struct {
	CallID       string   `json:"call_id"`
	ResourceURIs []string `json:"resource_uris,omitempty"`
}

// EvidenceScope is what evidence may legitimately point at: the tenant's
// objects and the tool results of this turn. A URL the model supplies is in neither.
type EvidenceScope struct {
	TenantID    int64
	ToolResults []ToolEvidence
}
