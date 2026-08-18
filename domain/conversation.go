package domain

import "time"

// TurnRequest contains one tenant-scoped end-user turn. Principal is the acting
// agent; OnBehalfOf names the human whose authority it exercises, if any.
type TurnRequest struct {
	TenantContext    TenantContext
	Principal        Principal
	DelegationBounds DelegationBounds
	WorkItemID       int64
	WorkItemDepth    int
	OnBehalfOf       PrincipalRef
	RequestID        string
	ConversationID   string
	AgentID          string
	Input            []byte
}

// TurnDispatch couples a durable turn with its short-lived reply route.
type TurnDispatch struct {
	Turn       TurnRequest
	ReplyRoute string
	EnqueuedAt time.Time
}

// TurnResult contains the response and usage for a completed turn.
type TurnResult struct {
	Response     []byte
	AgentVersion string
	CheckpointID string
	Usage        Usage
}

// TurnReply is an ordered response frame sent from a worker to conversation ingress.
type TurnReply struct {
	TenantID       int64
	RequestID      string
	ConversationID string
	ReplyRoute     string
	Sequence       int64
	Payload        []byte
	Final          bool
	ErrorCode      string
	AgentVersion   string
	EmittedAt      time.Time
}

// SessionSnapshot contains the latest durable conversation state. State is
// hydrated from StateRef by the durable store, which verifies the digest first.
type SessionSnapshot struct {
	ConversationID      string
	AgentVersion        string
	LatestTurnNo        int64
	LatestStepNo        int
	LastCompletedStepID string
	State               []byte
	StateRef            ObjectRef
	Revision            int64
}

// QueueMessage contains a durable turn delivery.
type QueueMessage struct {
	MessageID string
	Dispatch  TurnDispatch
	Attempt   int
}

// QueueLease represents a turn temporarily owned by a worker.
type QueueLease struct {
	Message  QueueMessage
	Deadline time.Time
}
