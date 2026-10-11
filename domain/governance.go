package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// GovernanceSection is the AgentDefinition.Sections entry holding a release's AgentGovernance.
const GovernanceSection = "governance"

// GovernanceField names one governance field of a Charter AgentDefinition by its Charter JSON name.
type GovernanceField string

const (
	GovernancePurpose              GovernanceField = "purpose"
	GovernanceAccountable          GovernanceField = "accountable"
	GovernanceResponsibilities     GovernanceField = "responsibilityIds"
	GovernanceTriggers             GovernanceField = "triggers"
	GovernanceRequiredInputs       GovernanceField = "requiredInputs"
	GovernanceExpectedOutcomes     GovernanceField = "expectedOutcomes"
	GovernanceCapabilities         GovernanceField = "capabilityIds"
	GovernancePolicies             GovernanceField = "policies"
	GovernanceConfidenceBoundaries GovernanceField = "confidenceBoundaries"
	GovernanceEscalationConditions GovernanceField = "escalationConditions"
)

// CharterGovernanceFields are the governance fields Charter's AgentDefinition schema requires, in its order.
func CharterGovernanceFields() []GovernanceField {
	return []GovernanceField{
		GovernancePurpose, GovernanceAccountable, GovernanceResponsibilities, GovernanceTriggers,
		GovernanceRequiredInputs, GovernanceExpectedOutcomes, GovernanceCapabilities, GovernancePolicies,
		GovernanceConfidenceBoundaries, GovernanceEscalationConditions,
	}
}

// TriggerChannel is how work reaches an agent.
type TriggerChannel string

const (
	TriggerConversation TriggerChannel = "conversation"
	TriggerMCP          TriggerChannel = "mcp"
	TriggerAPI          TriggerChannel = "api"
	TriggerSchedule     TriggerChannel = "schedule"
	TriggerEvent        TriggerChannel = "event"
)

// AgentTrigger declares one way work reaches an agent: Trigger is the Charter trigger it realizes,
// and a schedule or event names in Binding the product-owned schedule or topic, opaque to Scout.
type AgentTrigger struct {
	Trigger string         `json:"trigger"`
	Channel TriggerChannel `json:"channel"`
	Binding string         `json:"binding,omitempty"`
}

// CharterObjectRef is a Charter objectRef: a document of Kind, in Namespace when that is another
// namespace, or outside the Charter document set when External.
type CharterObjectRef struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Namespace string `json:"namespace,omitempty"`
	External  bool   `json:"external,omitempty"`
}

// CharterRef is a Charter idRef. Like Charter's own, it encodes as the identifier, or as
// {"namespace","id"} when Namespace is set; encoding/json needs the codec on the type.
type CharterRef struct {
	Namespace string
	ID        string
}

func (r CharterRef) MarshalJSON() ([]byte, error) {
	if r.Namespace == "" {
		return json.Marshal(r.ID)
	}
	return json.Marshal(map[string]string{"namespace": r.Namespace, "id": r.ID})
}

func (r *CharterRef) UnmarshalJSON(raw []byte) error {
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		*r = CharterRef{ID: id}
		return nil
	}
	var namespaced struct {
		Namespace string `json:"namespace"`
		ID        string `json:"id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&namespaced); err != nil {
		return fmt.Errorf("charter idRef is an identifier or {namespace, id}: %w", err)
	}
	*r = CharterRef{Namespace: namespaced.Namespace, ID: namespaced.ID}
	return nil
}

// AgentGovernance holds a Charter AgentDefinition's governance in Charter's field names and value
// shapes, plus how each Studio-declared trigger reaches the agent and which enterprise snapshot owns
// the fields an enterprise source supplies; neither of those two is a Charter member.
type AgentGovernance struct {
	Purpose              string                 `json:"purpose,omitempty"`
	Accountable          *CharterObjectRef      `json:"accountable,omitempty"`
	ResponsibilityIDs    []CharterRef           `json:"responsibilityIds,omitempty"`
	Triggers             []string               `json:"triggers,omitempty"`
	RequiredInputs       []string               `json:"requiredInputs,omitempty"`
	ExpectedOutcomes     []string               `json:"expectedOutcomes,omitempty"`
	CapabilityIDs        []CharterRef           `json:"capabilityIds,omitempty"`
	Policies             []string               `json:"policies,omitempty"`
	ConfidenceBoundaries []string               `json:"confidenceBoundaries,omitempty"`
	EscalationConditions []string               `json:"escalationConditions,omitempty"`
	TriggerChannels      []AgentTrigger         `json:"triggerChannels,omitempty"`
	Enterprise           *GovernanceSnapshotRef `json:"enterprise,omitempty"`
}

// GovernanceSnapshotRef is what a release freezes of enterprise-owned governance: the document,
// its version and digest, and the fields it supplied.
type GovernanceSnapshotRef struct {
	Document CharterObjectRef  `json:"document"`
	Version  string            `json:"version"`
	Digest   string            `json:"digest"`
	Fields   []GovernanceField `json:"fields"`
}

// GovernanceSnapshot is one version of an agent's enterprise-owned governance, such as a Charter
// AgentDefinition in an enterprise directory. Governance holds the Charter fields the enterprise
// owns; a field it leaves empty stays Studio's. Link is where a person reads or changes it.
type GovernanceSnapshot struct {
	Document   CharterObjectRef
	Version    string
	Digest     string
	Link       string
	Governance AgentGovernance
}

// AgentGovernanceView is an agent's governance as Studio shows it. Effective is what a publish
// would freeze now; Draft is what Studio keeps, without the fields the enterprise owns. Problems
// are what publication would refuse in Effective, short of checking trigger bindings exist.
type AgentGovernanceView struct {
	AgentID       string
	Effective     AgentGovernance
	Draft         AgentGovernance
	Link          string
	Problems      []AgentFieldError
	DraftRevision int64
}

// AgentGovernanceChange replaces the governance Studio keeps for an agent, behind its draft revision.
type AgentGovernanceChange struct {
	AgentID               string
	Governance            AgentGovernance
	ExpectedDraftRevision int64
}
