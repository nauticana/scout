package api

import (
	"encoding/json"
	"time"
)

const (
	// StudioGuardrailsPath serves GET (show) and POST (replace) of an agent's enforced rules.
	StudioGuardrailsPath   = StudioBasePath + "guardrails"
	StudioSafetyEventsPath = StudioBasePath + "safety-events"
)

// GuardrailRule is one enforced rule; params is the kind's JSON parameters.
type GuardrailRule struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Stages   []string        `json:"stages,omitempty"`
	Action   string          `json:"action"`
	Severity string          `json:"severity"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// InheritedGuardrails is one layer of rules an agent inherits: baseline, platform, tenant,
// or type. Digest is set for a platform or tenant restriction layer.
type InheritedGuardrails struct {
	Layer  string          `json:"layer"`
	Digest string          `json:"digest,omitempty"`
	Rules  []GuardrailRule `json:"rules"`
}

// AgentGuardrails are the layers an agent inherits, widest first, and its own rules, which only
// add rules or tighten an inherited rule of the same id; problem says why they no longer fit.
type AgentGuardrails struct {
	AgentName             string                `json:"agent_name"`
	Inherited             []InheritedGuardrails `json:"inherited"`
	Rules                 []GuardrailRule       `json:"rules"`
	Problem               string                `json:"problem,omitempty"`
	ExpectedAgentRevision int64                 `json:"expected_agent_revision"`
}

// AgentGuardrailChange replaces the agent's own rules as a whole; an empty list removes them.
type AgentGuardrailChange struct {
	AgentName             string          `json:"agent_name"`
	Rules                 []GuardrailRule `json:"rules"`
	ExpectedAgentRevision int64           `json:"expected_agent_revision"`
}

// SafetyEvent is one stored guardrail hit: rule ids and attribution, never the inspected
// content. Version is the agent release inspected and policy_version its guardrail version;
// next pages pass the last id as before.
type SafetyEvent struct {
	ID            int64     `json:"id"`
	Version       string    `json:"version,omitempty"`
	PolicyVersion string    `json:"policy_version,omitempty"`
	Stage         string    `json:"stage"`
	Layer         string    `json:"layer"`
	Action        string    `json:"action"`
	Severity      string    `json:"severity"`
	RuleIDs       []string  `json:"rule_ids"`
	ActorKind     string    `json:"actor_kind,omitempty"`
	ActorID       string    `json:"actor_id,omitempty"`
	RequestID     string    `json:"request_id,omitempty"`
	DurationMs    int64     `json:"duration_ms"`
	OccurredAt    time.Time `json:"occurred_at"`
}
