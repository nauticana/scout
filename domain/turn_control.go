package domain

import (
	"fmt"
	"strings"
	"time"
)

// OperationAuthorizationObject is the keel authorization object live turn operation is granted
// on, at agent:<id> like AGENT_STUDIO; no authoring action implies it.
const OperationAuthorizationObject = "AGENT_OPERATION"

// OperationAction is an AGENT_OPERATION action.
type OperationAction string

const (
	// OperationView shows an agent's live turns.
	OperationView OperationAction = "VIEW"
	// OperationOperate holds, takes over, acts in, hands back, finishes, and releases them, and
	// looks up their calls' outcomes.
	OperationOperate OperationAction = "OPERATE"
	// OperationPreempt holds a running turn at once, fencing out its worker.
	OperationPreempt OperationAction = "PREEMPT"
	// OperationReconcile attests an unknown call outcome.
	OperationReconcile OperationAction = "RECONCILE"
)

// TurnHoldCode says why a turn is held: a turn_hold_code code.
type TurnHoldCode string

const (
	// HoldCheckpoint is a hold taken at a durable checkpoint on request.
	HoldCheckpoint TurnHoldCode = "C"
	// HoldAgentPaused is a hold taken at once on a turn no worker ran when its agent was suspended.
	HoldAgentPaused TurnHoldCode = "A"
	// HoldPreempted is a hold an operator took at once, fencing out a running worker.
	HoldPreempted TurnHoldCode = "P"
	// HoldUnknownOutcome is a hold the worker took on a call whose outcome it could not establish.
	HoldUnknownOutcome TurnHoldCode = "U"
)

// Valid reports whether the code is a seeded turn_hold_code code.
func (code TurnHoldCode) Valid() bool {
	return code == HoldCheckpoint || code == HoldAgentPaused || code == HoldPreempted || code == HoldUnknownOutcome
}

// HoldMode is how an operator asks a turn to hold.
type HoldMode string

const (
	// HoldModeCheckpoint holds a queued or parked turn at once and a running one at its next durable checkpoint.
	HoldModeCheckpoint HoldMode = "checkpoint"
	// HoldModePreempt holds any live turn at once; a running worker is fenced out mid-step.
	HoldModePreempt HoldMode = "preempt"
)

// TurnClaim is the leased queue delivery a worker presents to acquire a turn. Only the
// current lease of the turn's queue row acquires it.
type TurnClaim struct {
	QueueID    int64
	LeaseToken int64
	WorkerID   string
	Dispatch   TurnDispatch
}

// Validate refuses a claim that names no lease.
func (claim TurnClaim) Validate() error {
	if claim.QueueID <= 0 || claim.LeaseToken == 0 || strings.TrimSpace(claim.WorkerID) == "" {
		return fmt.Errorf("%w: a turn claim needs a queue id, a lease token, and a worker id", ErrValidation)
	}
	return nil
}

// TurnOwner is the fence one acquisition of a turn returned. Every write the runtime makes
// for the turn presents it, and a store refuses it with ErrFenceLost once the fence moved on.
type TurnOwner struct {
	TenantID  int64
	RequestID string
	Fence     int64
}

// Check fails closed on an owner that names no turn or no fence.
func (owner TurnOwner) Check() error {
	if owner.TenantID <= 0 || strings.TrimSpace(owner.RequestID) == "" || owner.Fence <= 0 {
		return fmt.Errorf("%w: a turn owner with a tenant, a request, and a positive fence is required", ErrFenceLost)
	}
	return nil
}

// Owns reports whether the owner names the turn of tenantID and requestID.
func (owner TurnOwner) Owns(tenantID int64, requestID string) bool {
	return owner.TenantID == tenantID && owner.RequestID == requestID
}

// TurnRecord is the durable state of one turn the runtime reads.
type TurnRecord struct {
	TurnNo int64
	Status TurnStatus
	Fence  int64
	// Payload is the response of a completed turn or the error code of a failed or cancelled one.
	Payload []byte
}

// TurnHoldOutcome is what a hold request did: Status is TurnHeld when the turn was held at
// once, or the running status of a turn asked to hold at its next checkpoint.
type TurnHoldOutcome struct {
	Status TurnStatus
	Fence  int64
}

// Held reports whether the turn is held now.
func (outcome TurnHoldOutcome) Held() bool { return outcome.Status == TurnHeld }

// LiveTurn is one non-terminal turn of an agent as an operator sees it.
type LiveTurn struct {
	RequestID       string
	ConversationID  string
	TurnNo          int64
	Status          TurnStatus
	Fence           int64
	HoldCode        TurnHoldCode
	HoldRequested   bool
	HoldReason      string
	CancelRequested bool
	QueuedAt        time.Time
	StartedAt       time.Time
	HeldAt          time.Time
	// Holder is the person who operates the turn, or who finished it; HoldUntil ends their lease.
	Holder    PrincipalRef
	HoldUntil time.Time
	// Outcomes are the turn's dispatched calls and Entries its operator entries; only the
	// single-turn read fills them.
	Outcomes []CallOutcome
	Entries  []OperatorEntry
}

// LiveTurnFilter pages an agent's live turns, newest first; Before is the previous page's
// NextBefore cursor, empty for the first page, and Limit zero takes the default page size.
type LiveTurnFilter struct {
	AgentID string
	Before  string
	Limit   int
}

// LiveTurnPage is one page of live turns; NextBefore is empty on the last page.
type LiveTurnPage struct {
	Turns      []LiveTurn
	NextBefore string
}

// LiveTurnLimits bounds one Studio page of live turns.
type LiveTurnLimits struct {
	PageSize    int
	MaxPageSize int
}

// TurnHoldRequest asks one turn of an agent to hold.
type TurnHoldRequest struct {
	AgentID   string
	RequestID string
	Mode      HoldMode
	Reason    string
}

// TurnReleaseRequest returns a held turn to the queue; Fence is the held fence the operator saw.
type TurnReleaseRequest struct {
	AgentID   string
	RequestID string
	Fence     int64
}
