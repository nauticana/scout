package domain

import "time"

// RunStatus is the terminal outcome of one agent run: an agent_run_status code, equal to
// the terminal turn_status code of the same name.
type RunStatus string

const (
	RunCompleted RunStatus = RunStatus(TurnCompleted)
	RunFailed    RunStatus = RunStatus(TurnFailed)
	RunCancelled RunStatus = RunStatus(TurnCancelled)
)

// Valid reports whether the status is a seeded agent_run_status code.
func (status RunStatus) Valid() bool {
	return status == RunCompleted || status == RunFailed || status == RunCancelled
}

// AgentRun is one settled execution of an immutable release. RequestID is the
// durable turn's request id, empty for an execution outside a turn; a recorded
// request id is never recorded twice. Release.Digest, when set, must match the
// published definition.
type AgentRun struct {
	ID          int64
	Release     AgentReleaseReference
	TaskKind    string
	RequestID   string
	Status      RunStatus
	CompletedAt time.Time
}

// AgentRunFilter selects a page of runs, newest first. Empty fields do not
// filter. Before is exclusive: the page holds runs with an id below it, and zero
// starts at the newest; the next page passes the last run's ID.
type AgentRunFilter struct {
	AgentID   string
	RequestID string
	Status    RunStatus
	Before    int64
	Limit     int
}
