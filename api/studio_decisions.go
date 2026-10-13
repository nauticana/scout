package api

import "time"

const StudioDecisionsPath = StudioBasePath + "decisions"

// PrincipalRef names a principal and its optional display label.
type PrincipalRef struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// DecisionAuthority is the authority a decision exercised: a delegation grant and its grantor.
type DecisionAuthority struct {
	Subject PrincipalRef  `json:"subject"`
	GrantID string        `json:"grant_id,omitempty"`
	Grantor *PrincipalRef `json:"grantor,omitempty"`
}

// DecisionRecord is one governed decision of the tenant. Its evidence stays in object
// storage; only the digest is shown.
type DecisionRecord struct {
	ID             int64              `json:"id"`
	Category       string             `json:"category"`
	Action         string             `json:"action"`
	Resource       string             `json:"resource"`
	Outcome        string             `json:"outcome"`
	Reason         string             `json:"reason"`
	Principal      PrincipalRef       `json:"principal"`
	Authority      *DecisionAuthority `json:"authority,omitempty"`
	ClientRef      string             `json:"client_ref,omitempty"`
	ClientLabel    string             `json:"client_label,omitempty"`
	WorkRef        string             `json:"work_ref,omitempty"`
	ScopeID        string             `json:"scope_id,omitempty"`
	ReleaseVersion string             `json:"release_version,omitempty"`
	PolicyID       string             `json:"policy_id,omitempty"`
	PolicyVersion  string             `json:"policy_version,omitempty"`
	Obligations    []string           `json:"obligations,omitempty"`
	EvidenceDigest string             `json:"evidence_digest,omitempty"`
	RequestID      string             `json:"request_id"`
	ConversationID string             `json:"conversation_id,omitempty"`
	OccurredAt     time.Time          `json:"occurred_at"`
}

// DecisionPage is one page of records, newest first; next_before is absent on the last.
type DecisionPage struct {
	Records    []DecisionRecord `json:"records"`
	NextBefore int64            `json:"next_before,omitempty"`
}
