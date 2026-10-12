package api

import "time"

// Studio turn control paths: an agent's live turns newest first, one turn, holding and
// releasing it, and looking up or attesting the outcome of one of its calls.
const (
	StudioLiveTurnsPath   = StudioBasePath + "turns/live"
	StudioTurnPath        = StudioBasePath + "turn"
	StudioTurnHoldPath    = StudioBasePath + "turn/hold"
	StudioTurnReleasePath = StudioBasePath + "turn/release"
	StudioTurnLookupPath  = StudioBasePath + "turn/outcome/lookup"
	StudioTurnAttestPath  = StudioBasePath + "turn/outcome/attest"
)

// Attested outcomes of a call.
const (
	CallOutcomeCompleted = "completed"
	CallOutcomeFailed    = "failed"
)

// LiveTurn is one turn of an agent as an operator sees it. status is a turn_status code and
// hold_code a turn_hold_code code when held; fence is what a release names.
type LiveTurn struct {
	RequestID       string     `json:"request_id"`
	ConversationID  string     `json:"conversation_id"`
	TurnNo          int64      `json:"turn_no"`
	Status          string     `json:"status"`
	Fence           int64      `json:"fence"`
	HoldCode        string     `json:"hold_code,omitempty"`
	HoldRequested   bool       `json:"hold_requested"`
	HoldReason      string     `json:"hold_reason,omitempty"`
	CancelRequested bool       `json:"cancel_requested"`
	QueuedAt        time.Time  `json:"queued_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	HeldAt          *time.Time `json:"held_at,omitempty"`
	// Outcomes are the turn's dispatched calls; only the single-turn read returns them.
	Outcomes []CallOutcome `json:"outcomes,omitempty"`
}

// CallOutcome is one non-read-only call of a turn. status is a turn_call_outcome_status code
// (I in flight, A accepted, C completed, F failed, U unknown), resolution a turn_call_resolution
// code once settled; deadline_passed says a person may attest an open call now.
type CallOutcome struct {
	ExecutionStepID int64      `json:"execution_step_id"`
	CallID          string     `json:"call_id"`
	ToolID          string     `json:"tool_id"`
	ToolVersion     string     `json:"tool_version"`
	Status          string     `json:"status"`
	DispatchFence   int64      `json:"dispatch_fence"`
	DispatchedAt    time.Time  `json:"dispatched_at"`
	DeadlineAt      time.Time  `json:"deadline_at"`
	DeadlinePassed  bool       `json:"deadline_passed"`
	Resolution      string     `json:"resolution,omitempty"`
	ResolvedFence   int64      `json:"resolved_fence,omitempty"`
	ResolverKind    string     `json:"resolver_kind,omitempty"`
	ResolverID      string     `json:"resolver_id,omitempty"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	Note            string     `json:"note,omitempty"`
	Evidence        *Evidence  `json:"evidence,omitempty"`
}

// Evidence is an object a person cites: its uri and sha-256 digest.
type Evidence struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
}

// TurnOutcomeRequest names one call of a turn held or ended at fence; a stale fence is 409.
type TurnOutcomeRequest struct {
	AgentName       string `json:"agent_name"`
	RequestID       string `json:"request_id"`
	Fence           int64  `json:"fence"`
	ExecutionStepID int64  `json:"execution_step_id"`
	CallID          string `json:"call_id"`
}

// TurnAttestRequest attests the outcome, completed or failed, of one open call after its deadline;
// note is required and evidence optional.
type TurnAttestRequest struct {
	TurnOutcomeRequest
	Outcome  string    `json:"outcome"`
	Note     string    `json:"note"`
	Evidence *Evidence `json:"evidence,omitempty"`
}

// TurnAttestResult is the call's outcome after the attestation; awaits_second means a second
// authorized person must attest the same outcome before it settles.
type TurnAttestResult struct {
	Outcome      CallOutcome `json:"outcome"`
	AwaitsSecond bool        `json:"awaits_second"`
}

// LiveTurnPage is one page of live turns; next_before continues it and is empty on the last page.
type LiveTurnPage struct {
	Turns      []LiveTurn `json:"turns"`
	NextBefore string     `json:"next_before,omitempty"`
}

// TurnHoldRequest asks one turn to hold. mode checkpoint holds a queued or parked turn at once
// and a running one at its next durable checkpoint; mode preempt holds a running one at once too.
type TurnHoldRequest struct {
	AgentName string `json:"agent_name"`
	RequestID string `json:"request_id"`
	Mode      string `json:"mode"`
	Reason    string `json:"reason"`
}

// TurnHoldResult reports whether the turn is held now or asked to hold, and its fence.
type TurnHoldResult struct {
	Held   bool   `json:"held"`
	Status string `json:"status"`
	Fence  int64  `json:"fence"`
}

// TurnReleaseRequest returns a turn held at fence to the queue; a stale fence is 409.
type TurnReleaseRequest struct {
	AgentName string `json:"agent_name"`
	RequestID string `json:"request_id"`
	Fence     int64  `json:"fence"`
}
