package domain

import (
	"encoding/json"
	"time"
)

// AgentDefinition is an immutable version of a tenant agent.
type AgentDefinition struct {
	AgentID               string
	AgentKind             string
	Version               string
	Enabled               bool
	Models                AgentModelSelection
	ApprovalPolicy        AgentApprovalPolicy
	Languages             []CompiledPrompt
	Extension             json.RawMessage
	DefinitionDigest      string
	DraftRevision         int64
	PromptProfileRevision int64
	ChangeSummary         string
	PublishedBy           *int64
	RestoredFromVersion   string
	PublishedAt           time.Time
}
