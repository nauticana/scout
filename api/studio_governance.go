package api

import "encoding/json"

// StudioGovernancePath serves GET ?agent_name= and POST of an agent's governance.
const StudioGovernancePath = StudioBasePath + "governance"

// AgentGovernance is an agent's governance. governance and draft are fragments of a Charter
// AgentDefinition in Charter's own field names and value shapes, with triggerChannels added:
// governance is what a publish would freeze now, its enterprise member naming the snapshot that
// owns the fields listed there; draft is what Studio keeps, which a save sends back changed.
type AgentGovernance struct {
	AgentName             string            `json:"agent_name"`
	Governance            json.RawMessage   `json:"governance"`
	Draft                 json.RawMessage   `json:"draft"`
	EnterpriseLink        string            `json:"enterprise_link,omitempty"`
	Problems              []AgentFieldError `json:"problems"`
	ExpectedAgentRevision int64             `json:"expected_agent_revision"`
}

// AgentGovernanceChange replaces the governance Studio keeps for agent_name with draft.
type AgentGovernanceChange struct {
	AgentName             string          `json:"agent_name"`
	Draft                 json.RawMessage `json:"draft"`
	ExpectedAgentRevision int64           `json:"expected_agent_revision"`
}
