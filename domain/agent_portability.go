package domain

import "encoding/json"

const (
	// PortableAgentFormat versions the "scout:agent" extension of an exported agent.
	PortableAgentFormat = "scout.agent.v1"
	// DerivedFromTemplateKind and DerivedFromDefinitionKind are the Charter objectRef kinds a
	// derived agent names as its source; both are outside the Charter document set.
	DerivedFromTemplateKind   = "AgentTemplate"
	DerivedFromDefinitionKind = "AgentDefinition"
)

// DerivedFrom is Charter's AgentDefinition.derivedFrom (CHR-AGENT-010): the definition, template, or
// import a definition version came from. Digest is "sha256:<64 hex>".
type DerivedFrom struct {
	Source  CharterObjectRef `json:"source"`
	Version string           `json:"version"`
	Digest  string           `json:"digest"`
}

// PortableBinding is one agent-scope binding without tenant, scope, version, or author. A
// prompt_section binding names its section by Caption and Language, because section ids differ
// between environments; every other kind names ResourceID, which a singleton kind sets to the kind.
type PortableBinding struct {
	ResourceKind ResourceKind    `json:"resource_kind"`
	ResourceID   string          `json:"resource_id,omitempty"`
	Caption      string          `json:"caption,omitempty"`
	Language     string          `json:"language,omitempty"`
	MergeMode    MergeMode       `json:"merge_mode"`
	Sealed       bool            `json:"sealed,omitempty"`
	Value        json.RawMessage `json:"value"`
}

// PortableSetting is one frozen effective resource without its provenance, in frozen order. Value
// keeps the exact stored bytes, which the release's settings digest covers.
type PortableSetting struct {
	ResourceKind ResourceKind `json:"resource_kind"`
	ResourceID   string       `json:"resource_id"`
	Value        []byte       `json:"value"`
}

// PortableAgent is what an export carries and an import reads: the agent's draft parts, its own
// bindings and guardrail rules, and its active release with the settings frozen with it.
type PortableAgent struct {
	AgentTypeID string
	// TypeVersion is the agent's pinned type version, and TypeDigest that version's definition digest.
	TypeVersion string
	TypeDigest  string
	DisplayName string
	Models      AgentModelSelection
	Extension   json.RawMessage
	// Bindings are every platform kind bound at the agent's own scope, sorted.
	Bindings   []PortableBinding
	Guardrails []GuardrailRule
	// DraftOrigin is the draft's current origin; an import records its own instead.
	DraftOrigin *DerivedFrom
	// Release is the active release without its authors and scope provenance.
	Release  AgentDefinition
	Settings []PortableSetting
	// Namespace, EnterpriseID, SpecVersion, Document, and Digest are the envelope a decode read.
	Namespace    string
	EnterpriseID string
	SpecVersion  string
	Document     CharterObjectRef
	Digest       string
}

// AgentImportRequest creates an agent from an exported document, or with AgentID replaces that
// agent's draft settings and prompts. References map each knowledge and entitlement value the
// document names to a value of the importing tenant.
type AgentImportRequest struct {
	Document json.RawMessage
	AgentID  string
	// DisplayName and IdempotencyKey apply to a create; an empty name takes the document's.
	DisplayName    string
	IdempotencyKey string
	// ExpectedDraftRevision applies to a replace.
	ExpectedDraftRevision int64
	References            map[ResourceKind]map[string]string
}
