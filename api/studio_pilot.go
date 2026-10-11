package api

import "time"

const (
	StudioPilotPath       = StudioBasePath + "pilot"
	StudioPilotAddPath    = StudioBasePath + "pilot/add"
	StudioPilotRemovePath = StudioBasePath + "pilot/remove"
)

// PilotChangeRequest serves version of agent_name to one subject, or with remove stops it:
// subject_kind P (principal) names a person by decimal user id, O (position) the product's reference.
type PilotChangeRequest struct {
	AgentName   string `json:"agent_name"`
	Version     string `json:"version"`
	SubjectKind string `json:"subject_kind"`
	SubjectID   string `json:"subject_id"`
	Reason      string `json:"reason"`
}

// PilotMember is one membership of an agent's supervised pilot; valid_to is set once it ends.
type PilotMember struct {
	AgentName   string     `json:"agent_name"`
	Version     string     `json:"version"`
	SubjectKind string     `json:"subject_kind"`
	SubjectID   string     `json:"subject_id"`
	Reason      string     `json:"reason"`
	AddedByKind string     `json:"added_by_kind"`
	AddedByID   string     `json:"added_by_id"`
	ValidFrom   time.Time  `json:"valid_from"`
	ValidTo     *time.Time `json:"valid_to,omitempty"`
}
