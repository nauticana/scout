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
	// AliasID is the agent type alias the caller asked for; AgentID is the agent that
	// serves the turn: the alias target unless the caller named an agent of that type.
	AliasID string
	AgentID string
	// Release pins a new conversation to one immutable release instead of the agent's
	// traffic policy; it needs its own grant and ReleaseReason.
	Release       string
	ReleaseReason string
	// WorkRef is the product's opaque reference to the work the turn serves, such as a
	// task; usage and decision records carry it so cost reports per work item.
	WorkRef string
	Input   []byte
}

// SelectableAgent is one agent a caller may address through an agent type alias.
type SelectableAgent struct {
	AgentID     string
	DisplayName string
	// Default marks the alias target, which serves a turn that names no agent.
	Default bool
	Release string
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
	// Citations are the sources of the final step's grounded answer.
	Citations []Citation
	Usage     Usage
}

// TurnReply is an ordered response frame sent from a worker to conversation ingress.
type TurnReply struct {
	TenantID       int64
	RequestID      string
	ConversationID string
	ReplyRoute     string
	Sequence       int64
	Payload        []byte
	// Events are the typed view of the frame; Payload stays the opaque one.
	Events       []TurnEvent
	Final        bool
	ErrorCode    string
	AgentVersion string
	EmittedAt    time.Time
}

// SessionSnapshot contains the latest durable conversation state. State is
// hydrated from StateRef by the durable store, which verifies the digest first.
type SessionSnapshot struct {
	ConversationID string
	AgentVersion   string
	// EndUserRef is the human the conversation was opened for; empty when it has none.
	EndUserRef          string
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
