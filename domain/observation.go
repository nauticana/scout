package domain

import "time"

// ComponentVersions pins every versioned component that shaped one outcome.
type ComponentVersions struct {
	Agent     string
	Model     string
	Prompt    string
	Knowledge string
	Index     string
	Tool      string
	Guardrail string
	Evaluator string
	Release   string
}

// ObservationOutcome is the bounded result class of one stage.
type ObservationOutcome string

const (
	OutcomeOK       ObservationOutcome = "ok"
	OutcomeError    ObservationOutcome = "error"
	OutcomeRejected ObservationOutcome = "rejected"
	OutcomeDegraded ObservationOutcome = "degraded"
	OutcomeCanceled ObservationOutcome = "canceled"
)

// Observation is one stage- and version-attributed measurement of a turn.
// TenantID is for the ledger and tenant-scoped diagnostics; adapters label
// fleet series only with the bounded fields.
type Observation struct {
	TenantID int64
	// Principal and ScopeID are exact accounting dimensions for the tenant ledger,
	// never fleet metric labels.
	Principal      PrincipalRef
	ScopeID        string
	TenantTier     string
	PriorityClass  string
	Region         string
	Stage          TurnStage
	Component      string
	Versions       ComponentVersions
	Selection      ModelSelection
	StartedAt      time.Time
	Duration       time.Duration
	QueueWait      time.Duration
	TimeToFirst    time.Duration
	TimePerOutput  time.Duration
	Usage          Usage
	ReservedTokens int64
	ReservedCost   int64
	Outcome        ObservationOutcome
	ErrorClass     string
	TraceID        string
}

// ServingSignal is the per-route work and latency summary exported to a serving control plane.
type ServingSignal struct {
	Selection           ModelSelection
	WindowStart         time.Time
	Window              time.Duration
	QueuedPrefillTokens int64
	QueuedDecodeTokenS  int64
	QueueWaitP50        time.Duration
	QueueWaitP95        time.Duration
	TimeToFirstP95      time.Duration
	TimePerOutputP95    time.Duration
	AdmissionRejections int64
	CapacityOutcomes    map[string]int64
	KVPressure          float64
	Draining            bool
}
