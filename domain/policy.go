package domain

import "encoding/json"

// PolicyEffect is what a matching statement does.
type PolicyEffect string

const (
	PolicyAllow PolicyEffect = "allow"
	PolicyDeny  PolicyEffect = "deny"
)

// PolicyStatement is one rule in a compiled policy set. Actions and Resources
// match exactly or by a single trailing "*"; an empty list matches nothing.
type PolicyStatement struct {
	ID          string       `json:"id"`
	Effect      PolicyEffect `json:"effect"`
	Actions     []string     `json:"actions"`
	Resources   []string     `json:"resources"`
	Obligations []Obligation `json:"obligations,omitempty"`
	// Conditions are matched against DecisionSubject.Environment by exact value.
	Conditions json.RawMessage `json:"conditions,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

// PolicySet is the versioned, digest-addressed policy bound to a scope. Deny
// wins over allow regardless of statement order, and no match is a deny.
type PolicySet struct {
	PolicyID      string            `json:"policy_id"`
	Version       string            `json:"version"`
	SchemaVersion int               `json:"schema_version"`
	Statements    []PolicyStatement `json:"statements"`
}
