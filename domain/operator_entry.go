package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// OperatorEntryKind classifies one entry a person appends to a turn they took over: a
// turn_operator_entry_kind code.
type OperatorEntryKind string

const (
	// OperatorNote is a note for the agent.
	OperatorNote OperatorEntryKind = "N"
	// OperatorTool is a tool call the person made through the governed gateway.
	OperatorTool OperatorEntryKind = "T"
	// OperatorReply is a reply the person sent the end user.
	OperatorReply OperatorEntryKind = "Y"
	// OperatorWithdraw withdraws a call the agent proposed and never sent.
	OperatorWithdraw OperatorEntryKind = "W"
	// OperatorHandback is the instruction a person hands the turn back with.
	OperatorHandback OperatorEntryKind = "H"
)

// Valid reports whether the kind is a seeded turn_operator_entry_kind code.
func (kind OperatorEntryKind) Valid() bool {
	switch kind {
	case OperatorNote, OperatorTool, OperatorReply, OperatorWithdraw, OperatorHandback:
		return true
	}
	return false
}

// OperatorLease is the hold a takeover granted one person. Their writes present it and are
// refused with ErrFenceLost once the fence moved on or the lease lapsed by the store clock.
type OperatorLease struct {
	TenantID  int64
	RequestID string
	Fence     int64
	Holder    PrincipalRef
}

// Check fails closed on a lease that names no turn, fence, or person.
func (lease OperatorLease) Check() error {
	if lease.TenantID <= 0 || strings.TrimSpace(lease.RequestID) == "" || lease.Fence <= 0 ||
		lease.Holder.Kind != PrincipalHuman || strings.TrimSpace(lease.Holder.ID) == "" {
		return fmt.Errorf("%w: an operator lease names a tenant, a request, a positive fence, and its person", ErrFenceLost)
	}
	return nil
}

// OperatorHold is a turn a person operates: the lease and when it lapses by the store clock.
type OperatorHold struct {
	Lease     OperatorLease
	HoldUntil time.Time
}

// OperatorPosition is where an entry reaches the agent: StepNo is the turn's step that receives
// it, ExecutionStepID that step's id once it started, and AfterEntryNo its journal length at the
// hold. A step drains every entry of its number whose AfterEntryNo its journal cursor reached.
type OperatorPosition struct {
	StepNo          int
	ExecutionStepID int64
	AfterEntryNo    int
}

// OperatorToolUse is a person's tool call as its entry records it.
type OperatorToolUse struct {
	CallID    string            `json:"call_id"`
	ToolID    string            `json:"tool_id"`
	Version   string            `json:"tool_version"`
	Arguments json.RawMessage   `json:"arguments,omitempty"`
	Status    CallOutcomeStatus `json:"status"`
	Output    json.RawMessage   `json:"output,omitempty"`
	IsError   bool              `json:"is_error,omitempty"`
	// ResourceURIs are the evidence links the tool returned, which an evidenced reply may cite.
	ResourceURIs []string `json:"resource_uris,omitempty"`
}

// OperatorEntryBody is what one entry says; it is stored as the entry's object.
type OperatorEntryBody struct {
	// Text is a note, a reply, or a handback instruction.
	Text string           `json:"text,omitempty"`
	Tool *OperatorToolUse `json:"tool,omitempty"`
	// CallID names the proposed call a withdrawal withdraws.
	CallID string `json:"call_id,omitempty"`
	// Frame is the reply frame a reply is published as, at its sequence, by whoever publishes it.
	Frame *TurnReply `json:"frame,omitempty"`
}

// OperatorEntry is one immutable entry of a turn a person operates. Usage is what a tool call
// spent, settled against the tenant per action; Exponent is the decimal places of its currency.
type OperatorEntry struct {
	TenantID  int64
	RequestID string
	Fence     int64
	EntryNo   int
	Kind      OperatorEntryKind
	Actor     PrincipalRef
	Position  OperatorPosition
	Body      OperatorEntryBody
	Object    ObjectRef
	Usage     Usage
	Exponent  int
	CreatedAt time.Time
}

// OperatorSettings bounds operation: the lease a takeover or act grants, one entry's encoded
// size, the entries of one turn, and the sweeper's batch and period. Zero takes each default.
type OperatorSettings struct {
	Lease         time.Duration
	EntryMaxBytes int
	MaxEntries    int
	LapseBatch    int
	LapsePeriod   time.Duration
}

// TurnTakeoverRequest takes over a turn held at Fence.
type TurnTakeoverRequest struct {
	AgentID   string
	RequestID string
	Fence     int64
}

// TurnActRequest appends one entry to a turn the actor operates at Fence: a note or a reply
// carries Text, a tool call Tool and Arguments, and a withdrawal the CallID it withdraws.
type TurnActRequest struct {
	AgentID   string
	RequestID string
	Fence     int64
	Kind      OperatorEntryKind
	Text      string
	Tool      ToolReference
	Arguments []byte
	CallID    string
}

// TurnActResult is what one act did: its entry, its call's outcome for a tool call, the turn as it
// now stands, and whether a reply's frame reached the stream now rather than when the turn resumes.
type TurnActResult struct {
	Entry     OperatorEntry
	Outcome   *CallOutcome
	Turn      LiveTurn
	Published bool
}

// TurnHandbackRequest returns a turn the actor operates at Fence to the agent, with an optional instruction.
type TurnHandbackRequest struct {
	AgentID     string
	RequestID   string
	Fence       int64
	Instruction string
}

// TurnFinishRequest completes a turn the actor operates at Fence with their reply.
type TurnFinishRequest struct {
	AgentID   string
	RequestID string
	Fence     int64
	Reply     []byte
}

// TurnTakeover is a person taking a turn held at Fence.
type TurnTakeover struct {
	TenantID  int64
	RequestID string
	Fence     int64
	Holder    PrincipalRef
}

// OperatorCallIntent is a person's tool call recorded in flight before it leaves: its outcome key,
// idempotency key, the entry number it will be recorded as, and the window its deadline allows.
type OperatorCallIntent struct {
	Key            CallOutcomeKey
	IdempotencyKey string
	EntryNo        int
	Tool           ToolReference
	Window         time.Duration
}

// OperatorAct appends one entry under Lease. A reply's Body carries the frame it is published as;
// a tool call's entry names its Call intent and carries its definite Settlement, or none for a
// call nobody can establish, which holds the turn again. Reservation and Usage settle what the
// call spent in the entry's fenced transaction.
type OperatorAct struct {
	Lease       OperatorLease
	Kind        OperatorEntryKind
	Body        OperatorEntryBody
	Call        *OperatorCallIntent
	Settlement  *CallSettlement
	Reservation BudgetReservation
	Usage       Usage
}

// OperatorActed is an appended entry and, when an unknown call outcome held the turn, its held fence.
type OperatorActed struct {
	Entry     OperatorEntry
	HeldFence int64
}

// OperatorHandedBack is a handback: the queued fence it left the turn at and its instruction entry, if any.
type OperatorHandedBack struct {
	Fence       int64
	Instruction *OperatorEntry
}

// OperatorFinish completes a turn a person operates with Frame, the reply frame whose payload is
// the turn's response, settling the agent's held Reservation at what the turn's agent spent, in
// Currency when it priced nothing, and the operator entries' usage in one unit.
type OperatorFinish struct {
	Lease       OperatorLease
	Frame       TurnReply
	Reservation BudgetReservation
	Currency    string
	SubjectRef  string
	Attribution UsageAttribution
}

// LapsedHold is an operated turn whose lease lapsed and that the sweeper held again.
type LapsedHold struct {
	TenantID  int64
	RequestID string
	AgentID   string
	Fence     int64
	Holder    PrincipalRef
}
