package domain

import "encoding/json"

// TurnEventVersion is the wire version of the typed event vocabulary.
const TurnEventVersion = 1

// TurnEventKind names one typed event a turn can emit.
type TurnEventKind string

const (
	TurnEventTextDelta        TurnEventKind = "text_delta"
	TurnEventToolProposal     TurnEventKind = "tool_proposal"
	TurnEventToolResult       TurnEventKind = "tool_result"
	TurnEventApprovalPending  TurnEventKind = "approval_pending"
	TurnEventApprovalResolved TurnEventKind = "approval_resolved"
	TurnEventEvidence         TurnEventKind = "evidence"
	TurnEventProgress         TurnEventKind = "progress"
	TurnEventResult           TurnEventKind = "result"
	TurnEventExtension        TurnEventKind = "extension"
)

// TurnEvent is one typed, versioned event. Exactly the field its Kind names is
// set. Events travel on the TurnReply of the step that produced them, so they
// inherit its sequence, deduplication, and replay.
type TurnEvent struct {
	Version   int                 `json:"v"`
	Kind      TurnEventKind       `json:"kind"`
	Text      string              `json:"text,omitempty"`
	Tool      *TurnToolEvent      `json:"tool,omitempty"`
	Approval  *TurnApprovalEvent  `json:"approval,omitempty"`
	Evidence  []EvidenceRef       `json:"evidence,omitempty"`
	Progress  *TurnProgressEvent  `json:"progress,omitempty"`
	Extension *TurnExtensionEvent `json:"extension,omitempty"`
}

// TurnToolEvent describes a proposed call or its validated result.
type TurnToolEvent struct {
	CallID      string          `json:"call_id"`
	ToolID      string          `json:"tool_id"`
	ToolVersion string          `json:"tool_version"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	Output      json.RawMessage `json:"output,omitempty"`
	IsError     bool            `json:"is_error,omitempty"`
}

// TurnApprovalEvent reports a call parked for, or released by, a human decision.
type TurnApprovalEvent struct {
	CallID   string `json:"call_id,omitempty"`
	ToolID   string `json:"tool_id,omitempty"`
	Decision string `json:"decision,omitempty"`
}

// TurnProgressEvent is a coarse position report.
type TurnProgressEvent struct {
	Stage     string `json:"stage"`
	Iteration int    `json:"iteration,omitempty"`
	Message   string `json:"message,omitempty"`
}

// TurnExtensionEvent carries a product-defined payload under its own type and version.
type TurnExtensionEvent struct {
	Type    string          `json:"type"`
	Version int             `json:"v"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
