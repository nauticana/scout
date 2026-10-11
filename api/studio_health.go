package api

import "time"

// Studio health paths: one agent's health detail, and its alerts newest first.
const (
	StudioHealthPath = StudioBasePath + "health"
	StudioAlertsPath = StudioBasePath + "alerts"
)

// Telemetry states of an agent's health detail.
const (
	TelemetryFresh   = "fresh"
	TelemetryStale   = "stale"
	TelemetryMissing = "missing"
)

// HealthAlertPolicy is the effective health_alerts setting; zero is not evaluated.
type HealthAlertPolicy struct {
	WindowMinutes       int64 `json:"window_minutes"`
	MinTurns            int64 `json:"min_turns"`
	MaxFailureRateBP    int64 `json:"max_failure_rate_bp"`
	MaxP95LatencyMS     int64 `json:"max_p95_latency_ms"`
	MaxHardSafetyEvents int64 `json:"max_hard_safety_events"`
	MaxSilenceMinutes   int64 `json:"max_silence_minutes"`
}

// AgentHealth is an agent's production health over window_minutes ending at measured_at, read
// from exact turn and safety event records. turns_by_status counts turns by turn_status code;
// samples is completed plus failed turns, the failure rate's denominator. Durations and time to
// first response run from queueing over completed turns; null had no sample. telemetry is fresh,
// stale (the health evaluator has not evaluated the agent in time), or missing (no turn in the
// window); insufficient_samples means no rate or latency verdict may be read.
type AgentHealth struct {
	AgentName           string             `json:"agent_name"`
	WindowMinutes       int64              `json:"window_minutes"`
	MeasuredAt          time.Time          `json:"measured_at"`
	Telemetry           string             `json:"telemetry"`
	TurnsByStatus       map[string]int64   `json:"turns_by_status"`
	Samples             int64              `json:"samples"`
	InsufficientSamples bool               `json:"insufficient_samples"`
	FailureRateBP       *int64             `json:"failure_rate_bp"`
	DurationP50MS       *int64             `json:"duration_p50_ms"`
	DurationP95MS       *int64             `json:"duration_p95_ms"`
	FirstResponseP50MS  *int64             `json:"first_response_p50_ms"`
	FirstResponseP95MS  *int64             `json:"first_response_p95_ms"`
	LastCompletedAt     *time.Time         `json:"last_completed_at"`
	LastFailedAt        *time.Time         `json:"last_failed_at"`
	SafetyEvents        map[string]int64   `json:"safety_events"`
	OpenAlerts          []AgentAlert       `json:"open_alerts"`
	Policy              *HealthAlertPolicy `json:"policy"`
	PolicyProblem       string             `json:"policy_problem,omitempty"`
	LastEvaluatedAt     *time.Time         `json:"last_evaluated_at"`
}

// AgentAlert is one health alert. kind is an agent_alert_kind code (F failure rate, L latency,
// H hard safety events, S silence) and status an agent_alert_status code (O open, R resolved);
// observed and threshold are in the kind's unit: basis points, milliseconds, events, or, for
// silence, turns in the span and the span's minutes.
type AgentAlert struct {
	ID            int64      `json:"id"`
	Kind          string     `json:"kind"`
	Status        string     `json:"status"`
	OpenedAt      time.Time  `json:"opened_at"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	Observed      int64      `json:"observed"`
	Threshold     int64      `json:"threshold"`
	WindowMinutes int64      `json:"window_minutes"`
}

// AgentAlertPage is one page of an agent's alerts newest first; next pages pass next_before as before.
type AgentAlertPage struct {
	Alerts     []AgentAlert `json:"alerts"`
	NextBefore int64        `json:"next_before,omitempty"`
}
