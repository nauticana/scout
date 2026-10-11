package api

import "encoding/json"

const (
	// StudioExportPath serves GET export?agent_name=: the agent as a Charter AgentDefinition document.
	StudioExportPath = StudioBasePath + "export"
	// StudioImportPath serves POST import: a new agent, or with agent_name that agent's draft, from a document.
	StudioImportPath = StudioBasePath + "import"
)

// AgentImportRequest imports an exported document. A create takes idempotency_key and optionally
// display_name; a replace takes agent_name and expected_agent_revision. references map each knowledge
// and entitlement value the document names to a value of the importing tenant.
type AgentImportRequest struct {
	Document              json.RawMessage              `json:"document"`
	AgentName             string                       `json:"agent_name,omitempty"`
	DisplayName           string                       `json:"display_name,omitempty"`
	IdempotencyKey        string                       `json:"idempotency_key,omitempty"`
	ExpectedAgentRevision int64                        `json:"expected_agent_revision,omitempty"`
	References            map[string]map[string]string `json:"references,omitempty"`
}
