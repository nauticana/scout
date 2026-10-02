package domain

import "time"

// RunStatus is the terminal outcome of one agent run; the codes are turn_status's terminal codes.
type RunStatus string

const (
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

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
