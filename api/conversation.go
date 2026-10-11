package api

import "encoding/json"

const (
	ConversationDefaultBasePath = "/api/conversation/"
	ConversationTurnPath        = "turn"
	ConversationStreamPath      = "turn/stream"
	ConversationCancelPath      = "turn/cancel"
	ConversationAgentsPath      = "agents"
	ConversationFeedbackPath    = "turn/feedback"
)

// ConversationFeedbackRequest rates one completed turn from 1 to 5.
type ConversationFeedbackRequest struct {
	RequestID string `json:"request_id"`
	Rating    int    `json:"rating"`
	Reason    string `json:"reason,omitempty"`
}

// ConversationTurnRequest is the transport shape for one asynchronous turn. agent_type is
// the type alias; agent_name selects one agent of that type instead of its default, and
// release pins a new conversation to one release of it.
type ConversationTurnRequest struct {
	RequestID      string          `json:"request_id"`
	ConversationID string          `json:"conversation_id"`
	AgentType      string          `json:"agent_type"`
	AgentName      string          `json:"agent_name,omitempty"`
	Release        string          `json:"release,omitempty"`
	ReleaseReason  string          `json:"release_reason,omitempty"`
	WorkRef        string          `json:"work_ref,omitempty"`
	Input          json.RawMessage `json:"input"`
}

// ConversationAgent is one agent the caller may address through an agent type.
type ConversationAgent struct {
	AgentName   string `json:"agent_name"`
	DisplayName string `json:"display_name"`
	Default     bool   `json:"is_default"`
	Release     string `json:"release"`
}

// ConversationTurnAccepted acknowledges durable turn admission.
type ConversationTurnAccepted struct {
	RequestID string `json:"request_id"`
}

// ConversationCancelRequest identifies the turn to stop.
type ConversationCancelRequest struct {
	RequestID string `json:"request_id"`
	Reason    string `json:"reason,omitempty"`
}

// ConversationStreamError is the data of an SSE `error` event: a failure after the
// stream's 200 was committed, carrying the status it would otherwise have had.
type ConversationStreamError struct {
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id,omitempty"`
}
