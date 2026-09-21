package api

import "encoding/json"

const (
	ConversationDefaultBasePath = "/api/conversation/"
	ConversationTurnPath        = "turn"
	ConversationStreamPath      = "turn/stream"
	ConversationCancelPath      = "turn/cancel"
)

// ConversationTurnRequest is the transport shape for one asynchronous turn.
type ConversationTurnRequest struct {
	RequestID      string          `json:"request_id"`
	ConversationID string          `json:"conversation_id"`
	AgentID        string          `json:"agent_id"`
	Input          json.RawMessage `json:"input"`
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
