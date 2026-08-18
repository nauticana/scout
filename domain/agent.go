package domain

import (
	"encoding/json"
	"time"
)

// AgentDefinition is an immutable version of a tenant agent.
type AgentDefinition struct {
	AgentID               string              `json:"agent_id"`
	AgentTypeID           string              `json:"agent_type_id"`
	Version               string              `json:"version"`
	Enabled               bool                `json:"enabled"`
	Models                AgentModelSelection `json:"models"`
	ApprovalPolicy        AgentApprovalPolicy `json:"approval_policy"`
	Languages             []CompiledPrompt    `json:"languages"`
	Sources               []ResolvedPrompts   `json:"sources"`
	Extension             json.RawMessage     `json:"extension,omitempty"`
	DefinitionDigest      string              `json:"definition_digest"`
	DraftRevision         int64               `json:"draft_revision"`
	PromptProfileRevision int64               `json:"prompt_profile_revision"`
	ChangeSummary         string              `json:"change_summary,omitempty"`
	PublishedBy           *int64              `json:"published_by,omitempty"`
	RestoredFromVersion   string              `json:"restored_from_version,omitempty"`
	PublishedAt           time.Time           `json:"published_at"`
}
