package api

import "time"

// Studio turn control paths: an agent's live turns newest first, one turn, and holding and
// releasing it.
const (
	StudioLiveTurnsPath   = StudioBasePath + "turns/live"
	StudioTurnPath        = StudioBasePath + "turn"
	StudioTurnHoldPath    = StudioBasePath + "turn/hold"
	StudioTurnReleasePath = StudioBasePath + "turn/release"
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
}

// LiveTurnPage is one page of live turns; next_before continues it and is empty on the last page.
type LiveTurnPage struct {
	Turns      []LiveTurn `json:"turns"`
	NextBefore string     `json:"next_before,omitempty"`
}

// TurnHoldRequest asks one turn to hold. mode checkpoint holds a queued or parked turn at once
// and a running one at its next durable checkpoint.
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
