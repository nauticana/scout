package api

import "encoding/json"

// StudioRestrictionsPath serves GET, the restriction layers in force, and POST, which
// replaces the tenant's layer.
const StudioRestrictionsPath = StudioBasePath + "restrictions"

// RestrictionDenial is a standing deny statement: it refuses every action matching actions on
// every resource matching resources, exactly or by one trailing "*", whatever a release allows.
// Conditions, a flat string map, limit it to decisions whose environment holds those values.
type RestrictionDenial struct {
	ID         string          `json:"id"`
	Actions    []string        `json:"actions"`
	Resources  []string        `json:"resources"`
	Conditions json.RawMessage `json:"conditions,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

// RestrictionGuardrail is a guardrail rule that blocks or redacts; params is kind-specific.
type RestrictionGuardrail struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Stages   []string        `json:"stages,omitempty"`
	Action   string          `json:"action"`
	Severity string          `json:"severity"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// RestrictionLayer is one layer in force, in canonical order; digest is empty for a layer
// never written.
type RestrictionLayer struct {
	Digest     string                 `json:"digest"`
	Denials    []RestrictionDenial    `json:"denials"`
	Guardrails []RestrictionGuardrail `json:"guardrails"`
}

// RestrictedSetting is one effective setting of the agent a restriction overrides; members are
// the tools of a tool set it takes away or the allow statements of a policy it denies.
type RestrictedSetting struct {
	ResourceKind string   `json:"resource_kind"`
	ResourceID   string   `json:"resource_id"`
	Members      []string `json:"members"`
}

// RestrictionOverride is one restriction, by layer (platform or tenant), kind (denial or
// guardrail), and id, and the agent's effective settings it overrides; conditional marks a
// denial that applies only when a decision's environment holds its conditions.
type RestrictionOverride struct {
	Layer         string              `json:"layer"`
	Kind          string              `json:"kind"`
	RestrictionID string              `json:"restriction_id"`
	Conditional   bool                `json:"conditional"`
	Settings      []RestrictedSetting `json:"settings"`
}

// StudioRestrictions is the platform layer, read-only, and the tenant's, which POST replaces
// as a whole. With agent_name, overrides lists the restrictions that override the agent's
// effective settings, or settings_problem says why they do not compile.
type StudioRestrictions struct {
	Platform        RestrictionLayer      `json:"platform"`
	Tenant          RestrictionLayer      `json:"tenant"`
	AgentName       string                `json:"agent_name,omitempty"`
	Overrides       []RestrictionOverride `json:"overrides"`
	SettingsProblem string                `json:"settings_problem,omitempty"`
}

// RestrictionReplaceRequest replaces the tenant's layer as a whole when it is still at
// expected_digest, empty before the first write; the response is the layer written.
type RestrictionReplaceRequest struct {
	ExpectedDigest string                 `json:"expected_digest"`
	Denials        []RestrictionDenial    `json:"denials"`
	Guardrails     []RestrictionGuardrail `json:"guardrails"`
}
