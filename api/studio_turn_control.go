package api

import (
	"encoding/json"
	"time"
)

// Studio turn control paths: an agent's live turns newest first, one turn, holding and
// releasing it, and looking up or attesting the outcome of one of its calls.
const (
	StudioLiveTurnsPath   = StudioBasePath + "turns/live"
	StudioTurnPath        = StudioBasePath + "turn"
	StudioTurnHoldPath    = StudioBasePath + "turn/hold"
	StudioTurnReleasePath = StudioBasePath + "turn/release"
	StudioTurnLookupPath  = StudioBasePath + "turn/outcome/lookup"
	StudioTurnAttestPath  = StudioBasePath + "turn/outcome/attest"
	// StudioOpenOutcomesPath serves GET outcomes/open?agent_name=&before=&limit=.
	StudioOpenOutcomesPath = StudioBasePath + "outcomes/open"
)

// Attested outcomes of a call.
const (
	CallOutcomeCompleted = "completed"
	CallOutcomeFailed    = "failed"
)

// LiveTurn is one turn of an agent as an operator sees it. status is a turn_status code (O while a
// person operates it) and hold_code a turn_hold_code code when held; fence is what a release,
// takeover, act, handback, or finish names.
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
	// HolderKind and HolderID name the person who operates the turn, or who finished it;
	// hold_until ends their lease by the store clock.
	HolderKind string     `json:"holder_kind,omitempty"`
	HolderID   string     `json:"holder_id,omitempty"`
	HoldUntil  *time.Time `json:"hold_until,omitempty"`
	// Outcomes are the turn's dispatched calls and Entries its operator entries; only the
	// single-turn read returns them.
	Outcomes []CallOutcome   `json:"outcomes,omitempty"`
	Entries  []OperatorEntry `json:"entries,omitempty"`
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

// OpenOutcome is one in-flight (I), accepted (A), or unknown (U) call and the turn it holds;
// deadline_passed is read by the store clock and says a person may attest the call now.
type OpenOutcome struct {
	AgentName       string    `json:"agent_name"`
	RequestID       string    `json:"request_id"`
	ConversationID  string    `json:"conversation_id"`
	TurnNo          int64     `json:"turn_no"`
	ExecutionStepID int64     `json:"execution_step_id"`
	CallID          string    `json:"call_id"`
	ToolID          string    `json:"tool_id"`
	ToolVersion     string    `json:"tool_version"`
	Status          string    `json:"status"`
	DispatchedAt    time.Time `json:"dispatched_at"`
	DeadlineAt      time.Time `json:"deadline_at"`
	DeadlinePassed  bool      `json:"deadline_passed"`
}

// OpenOutcomePage is one page of open calls, oldest deadline first. next_before is an opaque
// cursor naming the page's last call: passed as before, the next page starts strictly after it
// in (deadline_at, request_id, execution_step_id, call_id) order. It is empty on the last page.
type OpenOutcomePage struct {
	Outcomes   []OpenOutcome `json:"outcomes"`
	NextBefore string        `json:"next_before,omitempty"`
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

// Studio turn operation paths: taking a held turn over, acting in it, and handing it back or finishing it.
const (
	StudioTurnTakeoverPath = StudioBasePath + "turn/takeover"
	StudioTurnActPath      = StudioBasePath + "turn/act"
	StudioTurnHandbackPath = StudioBasePath + "turn/handback"
	StudioTurnFinishPath   = StudioBasePath + "turn/finish"
)

// Kinds of an operator act.
const (
	OperatorActNote     = "note"
	OperatorActTool     = "tool"
	OperatorActReply    = "reply"
	OperatorActWithdraw = "withdraw"
)

// TurnTakeoverRequest takes over a turn held at fence; a stale fence or an open call outcome is 409.
type TurnTakeoverRequest struct {
	AgentName string `json:"agent_name"`
	RequestID string `json:"request_id"`
	Fence     int64  `json:"fence"`
}

// TurnActRequest appends one entry to a turn the caller operates at fence and renews their lease.
// kind is note or reply with text, tool with tool_id, tool_version, and arguments, or withdraw with
// the call_id of a call the agent proposed and never sent. A lapsed lease or a stale fence is 409.
type TurnActRequest struct {
	AgentName   string          `json:"agent_name"`
	RequestID   string          `json:"request_id"`
	Fence       int64           `json:"fence"`
	Kind        string          `json:"kind"`
	Text        string          `json:"text,omitempty"`
	ToolID      string          `json:"tool_id,omitempty"`
	ToolVersion string          `json:"tool_version,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	CallID      string          `json:"call_id,omitempty"`
}

// TurnActResult is the entry an act appended, its call's outcome for a tool call, the turn as it
// now stands, and whether a reply reached the end user's stream now rather than when the turn resumes.
type TurnActResult struct {
	Entry     OperatorEntry `json:"entry"`
	Outcome   *CallOutcome  `json:"outcome,omitempty"`
	Turn      LiveTurn      `json:"turn"`
	Published bool          `json:"published"`
}

// TurnHandbackRequest returns a turn the caller operates at fence to its agent, with an optional
// instruction the agent's next step reads; an open call outcome is 409.
type TurnHandbackRequest struct {
	AgentName   string `json:"agent_name"`
	RequestID   string `json:"request_id"`
	Fence       int64  `json:"fence"`
	Instruction string `json:"instruction,omitempty"`
}

// TurnFinishRequest completes a turn the caller operates at fence with their reply, which passes
// the release's output guardrails and evidence rule; an open call outcome is 409.
type TurnFinishRequest struct {
	AgentName string `json:"agent_name"`
	RequestID string `json:"request_id"`
	Fence     int64  `json:"fence"`
	Reply     string `json:"reply"`
}

// OperatorEntry is one entry a person made in a turn they operated. kind is a
// turn_operator_entry_kind code (N note, T tool call, Y reply, W withdrawal, H handback
// instruction); step_no and after_entry_no say where the agent reads it. A tool call's spend is in
// minor units of currency, which has exponent decimal places.
type OperatorEntry struct {
	Fence           int64            `json:"fence"`
	EntryNo         int              `json:"entry_no"`
	Kind            string           `json:"kind"`
	ActorKind       string           `json:"actor_kind"`
	ActorID         string           `json:"actor_id"`
	StepNo          int              `json:"step_no"`
	ExecutionStepID int64            `json:"execution_step_id,omitempty"`
	AfterEntryNo    int              `json:"after_entry_no"`
	Text            string           `json:"text,omitempty"`
	Tool            *OperatorToolUse `json:"tool,omitempty"`
	CallID          string           `json:"call_id,omitempty"`
	Reply           string           `json:"reply,omitempty"`
	ReplySequence   *int64           `json:"reply_sequence,omitempty"`
	InputTokens     int64            `json:"input_tokens,omitempty"`
	OutputTokens    int64            `json:"output_tokens,omitempty"`
	ToolCalls       int              `json:"tool_calls,omitempty"`
	SearchQueries   int64            `json:"search_queries,omitempty"`
	CostMinorUnits  int64            `json:"cost_minor_units,omitempty"`
	Currency        string           `json:"currency,omitempty"`
	Exponent        int              `json:"exponent,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
}

// OperatorToolUse is a person's tool call: status is a turn_call_outcome_status code.
type OperatorToolUse struct {
	CallID       string          `json:"call_id"`
	ToolID       string          `json:"tool_id"`
	ToolVersion  string          `json:"tool_version"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	Status       string          `json:"status"`
	Output       json.RawMessage `json:"output,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	ResourceURIs []string        `json:"resource_uris,omitempty"`
}
