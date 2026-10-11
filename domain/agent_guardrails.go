package domain

// GuardrailLayerType is the rules an agent's type declares. Publication freezes them into
// the release's own rule set, so the enforcer attributes their hits to the release layer.
const GuardrailLayerType GuardrailLayer = "type"

// InheritedGuardrails is one layer of enforced rules an agent inherits: the operator
// baseline, the platform or tenant restriction layer, or its type. Digest is set for a
// restriction layer.
type InheritedGuardrails struct {
	Layer  GuardrailLayer
	Digest string
	Rules  []GuardrailRule
}

// AgentGuardrails are an agent's enforced guardrail rules: the layers it inherits, widest
// first, and its own rules behind the draft revision. Its own rules only add to what it
// inherits or tighten an inherited rule of the same id. Problem names why they no longer
// fit what the agent inherits; publication refuses them then.
type AgentGuardrails struct {
	AgentID       string
	Inherited     []InheritedGuardrails
	Rules         []GuardrailRule
	Problem       string
	DraftRevision int64
}

// AgentGuardrailChange replaces an agent's own rules as a whole; no rules removes them.
type AgentGuardrailChange struct {
	AgentID               string
	Rules                 []GuardrailRule
	ExpectedDraftRevision int64
}

// SafetyEventFilter pages one agent's safety events newest first. Before is the id of the
// last event already seen, zero for the first page.
type SafetyEventFilter struct {
	AgentID string
	Before  int64
	Limit   int
}
