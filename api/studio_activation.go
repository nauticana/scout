package api

import "time"

const (
	StudioActivationsPath      = StudioBasePath + "activations"
	StudioActivationCancelPath = StudioBasePath + "activation/cancel"
)

// ReleaseActivation is a scheduled publish: the release that takes the agent's traffic
// at effective_at. State is pending, activated, refused, or cancelled; reason and detail
// say why a refused one did not activate.
type ReleaseActivation struct {
	ID               int64      `json:"id"`
	AgentName        string     `json:"agent_name"`
	Version          int64      `json:"version"`
	DefinitionDigest string     `json:"definition_digest"`
	CanaryPercentage int        `json:"canary_percentage,omitempty"`
	EffectiveAt      time.Time  `json:"effective_at"`
	State            string     `json:"state"`
	ScheduledBy      string     `json:"scheduled_by"`
	ScheduledAt      time.Time  `json:"scheduled_at"`
	Attempts         int        `json:"attempts"`
	NextAttemptAt    *time.Time `json:"next_attempt_at,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	Detail           string     `json:"detail,omitempty"`
	DecidedBy        string     `json:"decided_by,omitempty"`
	DecidedAt        *time.Time `json:"decided_at,omitempty"`
}

// ReleaseActivationCancel withdraws a pending activation.
type ReleaseActivationCancel struct {
	AgentName    string `json:"agent_name"`
	ActivationID int64  `json:"activation_id"`
	Reason       string `json:"reason,omitempty"`
}
