package domain

import "time"

// PilotSubjectKind names who a pilot membership lists.
type PilotSubjectKind string

const (
	// PilotPrincipal lists one person by decimal user_account id.
	PilotPrincipal PilotSubjectKind = "P"
	// PilotPosition lists whoever the product says holds a position; Scout keeps the
	// reference opaque, because positions are the product's vocabulary.
	PilotPosition PilotSubjectKind = "O"
)

// PilotSubject is the person or position one pilot membership lists.
type PilotSubject struct {
	Kind PilotSubjectKind `json:"kind"`
	ID   string           `json:"id"`
}

// PilotMember serves one release of an agent to a listed subject. Membership is explicit
// and effective-dated, unlike a hash-based experiment cohort; ValidTo is zero while it is open.
type PilotMember struct {
	TenantID  int64
	AgentID   string
	Version   string
	Subject   PilotSubject
	Reason    string
	AddedBy   PrincipalRef
	ValidFrom time.Time
	ValidTo   time.Time
}

// PilotChange adds or removes one pilot membership; Reason is recorded with the change.
type PilotChange struct {
	AgentID string
	Version string
	Subject PilotSubject
	Reason  string
}
