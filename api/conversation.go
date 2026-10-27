package api

import (
	"encoding/json"
	"time"
)

const (
	ConversationDefaultBasePath = "/api/conversation/"
	ConversationTurnPath        = "turn"
	ConversationStreamPath      = "turn/stream"
	ConversationCancelPath      = "turn/cancel"
	ConversationAgentsPath      = "agents"
	ConversationFeedbackPath    = "turn/feedback"
	ConversationTurnsPath       = "turns"
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
	Status    int32  `json:"status"`
	Detail    string `json:"detail"`
	RequestID string `json:"request_id,omitempty"`
}

// ConversationTurn is one turn of the signed-in person's history. cost_minor is the settled
// cost in minor units of currency at exponent decimal places; currency and exponent are absent
// while nothing is settled.
type ConversationTurn struct {
	RequestID      string          `json:"request_id"`
	ConversationID string          `json:"conversation_id"`
	AgentName      string          `json:"agent_name"`
	ReleaseVersion string          `json:"release_version"`
	Status         string          `json:"status"`
	WorkRef        string          `json:"work_ref,omitempty"`
	TaskKind       string          `json:"task_kind"`
	InputSummary   string          `json:"input_summary"`
	ErrorText      string          `json:"error_text,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	CostMinor      *int64          `json:"cost_minor,omitempty"`
	Currency       string          `json:"currency,omitempty"`
	Exponent       *int            `json:"exponent,omitempty"`
}

// ConversationTurnPage is one page of turns, newest first; next_before is empty on the last.
type ConversationTurnPage struct {
	Turns      []ConversationTurn `json:"turns"`
	NextBefore string             `json:"next_before,omitempty"`
}
