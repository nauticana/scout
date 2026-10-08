package domain

import (
	"encoding/json"
	"time"
)

// MCPConfirmationStatus is the lifecycle of one confirmed MCP tool call.
type MCPConfirmationStatus string

const (
	MCPConfirmationPending   MCPConfirmationStatus = "pending"
	MCPConfirmationApproved  MCPConfirmationStatus = "approved"
	MCPConfirmationExecuting MCPConfirmationStatus = "executing"
	MCPConfirmationExecuted  MCPConfirmationStatus = "executed"
	MCPConfirmationFailed    MCPConfirmationStatus = "failed"
	MCPConfirmationUnknown   MCPConfirmationStatus = "unknown"
	MCPConfirmationDeclined  MCPConfirmationStatus = "declined"
	MCPConfirmationWithdrawn MCPConfirmationStatus = "withdrawn"
	MCPConfirmationExpired   MCPConfirmationStatus = "expired"
)

// MCPConfirmationChannel is how a person is asked to confirm.
type MCPConfirmationChannel string

const (
	// MCPConfirmationElicitation asks the maker through an MCP elicitation form.
	MCPConfirmationElicitation MCPConfirmationChannel = "elicitation"
	// MCPConfirmationInbox waits for an authorized person outside the MCP client.
	MCPConfirmationInbox MCPConfirmationChannel = "inbox"
)

// MCPConfirmationDraft is an MCP tool call as first prepared. Payload is exactly
// what will run; Digest identifies the action and guards against preparing it
// twice. Requirements is the product's authorization data, re-checked at run time.
type MCPConfirmationDraft struct {
	TenantID     int64
	Maker        PrincipalRef
	ClientRef    string
	Tool         string
	Digest       string
	Payload      json.RawMessage
	Preview      json.RawMessage
	Requirements json.RawMessage
	Channel      MCPConfirmationChannel
	// ApprovalRequired forbids direct decisions: only an attached keel
	// maker-checker request decides it.
	ApprovalRequired bool
	TTL              time.Duration
}

// MCPConfirmation is one stored confirmation. Expired reports a pending one past
// ExpiresAt on the store clock.
type MCPConfirmation struct {
	ID, TenantID     int64
	Maker            PrincipalRef
	ClientRef        string
	Tool             string
	PayloadDigest    string
	Payload          json.RawMessage
	Preview          json.RawMessage
	Requirements     json.RawMessage
	Channel          MCPConfirmationChannel
	Status           MCPConfirmationStatus
	ApprovalID       int64
	ExpiresAt        time.Time
	CreatedAt        time.Time
	DecidedAt        time.Time
	Decider          PrincipalRef
	DecisionNote     string
	Fence            int64
	Attempts         int64
	Result           json.RawMessage
	Error            string
	CompletedAt      time.Time
	Reconciler       PrincipalRef
	ReconcileNote    string
	ApprovalRequired bool
	Expired          bool
}

// MCPConfirmationKey names one confirmation.
type MCPConfirmationKey struct {
	TenantID, ID int64
}

// ExpiredMCPConfirmation is what expiry closed; the product closes ApprovalID
// and tells the maker.
type ExpiredMCPConfirmation struct {
	MCPConfirmationKey
	Maker      PrincipalRef
	Tool       string
	ApprovalID int64
}
