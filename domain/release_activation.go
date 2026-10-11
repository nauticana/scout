package domain

import "time"

// ReleaseActivationState is the lifecycle code of a scheduled release activation.
type ReleaseActivationState string

const (
	ActivationPending   ReleaseActivationState = "pending"
	ActivationActivated ReleaseActivationState = "activated"
	ActivationRefused   ReleaseActivationState = "refused"
	ActivationCancelled ReleaseActivationState = "cancelled"
)

// ReleaseActivationReason says why an activation was refused.
type ReleaseActivationReason string

const (
	ActivationVersionMissing    ReleaseActivationReason = "VM"
	ActivationDigestMismatch    ReleaseActivationReason = "DM"
	ActivationNotAuthorized     ReleaseActivationReason = "NA"
	ActivationAgentArchived     ReleaseActivationReason = "AA"
	ActivationGuardRefused      ReleaseActivationReason = "GR"
	ActivationDeploymentChanged ReleaseActivationReason = "DC"
	ActivationAttemptsExhausted ReleaseActivationReason = "AE"
)

// ReleaseActivation is a published release waiting to take an agent's traffic at
// EffectiveAt. BaseStable and BaseCanary are the deployment it was scheduled over:
// it activates only while the deployment still serves them.
type ReleaseActivation struct {
	ID               int64
	TenantID         int64
	AgentID          string
	Version          string
	DefinitionDigest string
	// CanaryPercentage between 1 and 99 activates the release as a canary; 0 makes it stable.
	CanaryPercentage int
	BaseStable       string
	BaseCanary       string
	EffectiveAt      time.Time
	State            ReleaseActivationState
	ScheduledBy      PrincipalRef
	ScheduledAt      time.Time
	Attempts         int
	NextAttemptAt    *time.Time
	Reason           ReleaseActivationReason
	Detail           string
	DecidedBy        *PrincipalRef
	DecidedAt        *time.Time
}

// ReleaseActivationCancel withdraws a pending activation before it activates.
type ReleaseActivationCancel struct {
	AgentID      string
	ActivationID int64
	Reason       string
}

// ReleaseActivationSettings bound the activation worker: Batch activations per poll,
// one poll every PollPeriod, MaxAttempts deliveries of a transient failure before the
// activation is refused, and Timeout on one activation's checks and swap, zero taking
// the activation service's default.
type ReleaseActivationSettings struct {
	Batch       int
	PollPeriod  time.Duration
	MaxAttempts int
	Timeout     time.Duration
}

// ReleaseActivationReport counts one pass of the activation worker.
type ReleaseActivationReport struct {
	Activated int
	Refused   int
	Deferred  int
}
