package domain

import "time"

// Bounds of a health_alerts value.
const (
	// MaxHealthWindowMinutes bounds the measurement window and the silence span: one week.
	MaxHealthWindowMinutes = 10080
	// MaxHealthLatencyMS bounds a p95 latency threshold: one day.
	MaxHealthLatencyMS = 86_400_000
	// MaxHealthCount bounds the sample floor and the hard safety event threshold.
	MaxHealthCount = 1_000_000
	// MaxHealthRateBP is a whole rate in basis points.
	MaxHealthRateBP = 10000
)

// HealthAlertPolicy is the health_alerts setting: when an agent's production health opens an
// alert. Every field is an integer and zero is not evaluated. WindowMinutes is the measurement
// window (zero takes the evaluator's default); MinTurns the finished turns below which the
// failure rate and latency are not judged; the Max fields are the thresholds a measurement must
// exceed to open an alert; MaxSilenceMinutes the longest an active agent with a stable release
// may go without a turn; MaxUnresolvedMinutes the longest any turn of the agent may stay held
// with an open call outcome.
type HealthAlertPolicy struct {
	WindowMinutes        int64 `json:"window_minutes"`
	MinTurns             int64 `json:"min_turns"`
	MaxFailureRateBP     int64 `json:"max_failure_rate_bp"`
	MaxP95LatencyMS      int64 `json:"max_p95_latency_ms"`
	MaxHardSafetyEvents  int64 `json:"max_hard_safety_events"`
	MaxSilenceMinutes    int64 `json:"max_silence_minutes"`
	MaxUnresolvedMinutes int64 `json:"max_unresolved_minutes"`
}

// Evaluates reports whether the policy sets any threshold.
func (p HealthAlertPolicy) Evaluates() bool {
	return p.MaxFailureRateBP > 0 || p.MaxP95LatencyMS > 0 || p.MaxHardSafetyEvents > 0 || p.MaxSilenceMinutes > 0 ||
		p.MaxUnresolvedMinutes > 0
}

// SampleFloor is the fewest finished turns a rate or latency judgment needs; never below one.
func (p HealthAlertPolicy) SampleFloor() int64 { return max(p.MinTurns, 1) }

// AgentAlertKind is what an agent alert reports: an agent_alert_kind code.
type AgentAlertKind string

const (
	AlertFailureRate AgentAlertKind = "F"
	AlertLatency     AgentAlertKind = "L"
	AlertSafety      AgentAlertKind = "H"
	// AlertSilence is missing telemetry: an active agent served no turn for its silence span.
	AlertSilence AgentAlertKind = "S"
	// AlertUnresolved is a turn held longer than the policy allows with an open call outcome.
	AlertUnresolved AgentAlertKind = "U"
)

// Valid reports whether the kind is a seeded agent_alert_kind code.
func (kind AgentAlertKind) Valid() bool {
	return kind == AlertFailureRate || kind == AlertLatency || kind == AlertSafety || kind == AlertSilence || kind == AlertUnresolved
}

// AgentAlertStatus is an agent alert's lifecycle: an agent_alert_status code.
type AgentAlertStatus string

const (
	AlertOpen     AgentAlertStatus = "O"
	AlertResolved AgentAlertStatus = "R"
)

// Valid reports whether the status is a seeded agent_alert_status code.
func (status AgentAlertStatus) Valid() bool { return status == AlertOpen || status == AlertResolved }

// AgentAlert is one health alert of an agent. Observed and Threshold are in the kind's unit:
// basis points, milliseconds, events, for silence turns in the silence span and its minutes, and
// for unresolved outcomes the turns held that long with one and the minutes.
type AgentAlert struct {
	ID            int64
	TenantID      int64
	AgentID       string
	Kind          AgentAlertKind
	Status        AgentAlertStatus
	OpenedAt      time.Time
	ResolvedAt    time.Time
	Observed      int64
	Threshold     int64
	WindowMinutes int64
}

// AgentAlertFilter pages one agent's alerts newest first; an empty Status lists every status.
type AgentAlertFilter struct {
	AgentID string
	Status  AgentAlertStatus
	Before  int64
	Limit   int
}

// AgentAlertPage is one page of alerts; NextBefore continues it and is zero on the last page.
type AgentAlertPage struct {
	Alerts     []AgentAlert
	NextBefore int64
}

// TelemetryState says whether an agent's health can be trusted: fresh, stale when the health
// evaluator owes the agent an evaluation it has not made in time, or missing when the window
// holds no turn.
type TelemetryState string

const (
	TelemetryFresh   TelemetryState = "fresh"
	TelemetryStale   TelemetryState = "stale"
	TelemetryMissing TelemetryState = "missing"
)

// AgentHealthMeasure is an agent's exact turn and safety records over a window, measured by the
// database clock. Turns count by their queue time; durations and time to first response run from
// queueing and cover completed turns only; a nil percentile had no sample.
type AgentHealthMeasure struct {
	WindowMinutes      int64
	MeasuredAt         time.Time
	TurnsByStatus      map[TurnStatus]int64
	DurationP50MS      *int64
	DurationP95MS      *int64
	FirstResponseP50MS *int64
	FirstResponseP95MS *int64
	LastCompletedAt    time.Time
	LastFailedAt       time.Time
	SafetyBySeverity   map[GuardrailSeverity]int64
}

// Turns is every turn queued in the window.
func (m AgentHealthMeasure) Turns() int64 {
	var total int64
	for _, count := range m.TurnsByStatus {
		total += count
	}
	return total
}

// Judged is the finished turns a failure rate is judged over: completed and failed, not cancelled.
func (m AgentHealthMeasure) Judged() int64 {
	return m.TurnsByStatus[TurnCompleted] + m.TurnsByStatus[TurnFailed]
}

// FailureRateBP is failed over judged turns in basis points; false without a judged turn.
func (m AgentHealthMeasure) FailureRateBP() (int64, bool) {
	judged := m.Judged()
	if judged == 0 {
		return 0, false
	}
	return m.TurnsByStatus[TurnFailed] * MaxHealthRateBP / judged, true
}

// AgentHealthReport is Studio's health detail of one agent. Policy is the effective
// health_alerts setting, nil when none is bound or the chain does not compile (PolicyProblem
// says why). InsufficientSamples reports fewer judged turns than the policy's sample floor,
// so no rate or latency verdict may be read from the measure. OpenOutcomes counts the in-flight,
// accepted, and unknown calls of every live or ended turn of the agent, whenever queued.
type AgentHealthReport struct {
	AgentID             string
	Measure             AgentHealthMeasure
	OpenOutcomes        int64
	Policy              *HealthAlertPolicy
	PolicyProblem       string
	InsufficientSamples bool
	Telemetry           TelemetryState
	LastEvaluatedAt     time.Time
	OpenAlerts          []AgentAlert
}

// AgentHealthSettings bound the health evaluator and Studio's health views. Zero takes each default.
type AgentHealthSettings struct {
	// Period is the evaluator's interval between passes.
	Period time.Duration
	// Batch is the active agents one page of a pass reads.
	Batch int
	// WindowMinutes is the measurement window of a policy that names none.
	WindowMinutes int64
	// StalePeriods is how many periods may pass without an evaluation before telemetry is stale.
	StalePeriods int
	// AlertPageSize and AlertMaxPageSize bound one Studio alert page.
	AlertPageSize    int
	AlertMaxPageSize int
}

// HealthEvaluationReport counts one evaluator pass: agents evaluated, skipped without a policy
// that evaluates anything, alerts opened and resolved, opened alerts not delivered, and agents
// whose evaluation failed.
type HealthEvaluationReport struct {
	Evaluated   int
	Skipped     int
	Opened      int
	Resolved    int
	Undelivered int
	Failed      int
}
