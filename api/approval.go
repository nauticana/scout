package api

import (
	"encoding/json"
	"time"
)

const (
	ApprovalBasePath    = "/api/approval/"
	ApprovalPendingPath = ApprovalBasePath + "pending"
	ApprovalResolvePath = ApprovalBasePath + "resolve"
	ApprovalLinkPath    = ApprovalBasePath + "link"
)

// ApprovalRequest is one decision a reviewer owes; evidence stays behind its own authorization.
type ApprovalRequest struct {
	RequestID       string     `json:"request_id"`
	ExecutionStepID int64      `json:"execution_step_id"`
	ConversationID  string     `json:"conversation_id"`
	ScopeID         string     `json:"scope_id,omitempty"`
	RuleID          string     `json:"rule_id,omitempty"`
	Action          string     `json:"action"`
	Resource        string     `json:"resource"`
	OutputClass     string     `json:"output_class"`
	RiskTier        string     `json:"risk_tier"`
	Summary         string     `json:"summary"`
	ProposedDigest  string     `json:"proposed_digest"`
	Status          string     `json:"status"`
	DeadlineAt      *time.Time `json:"deadline_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	// RevisesStepID names the edited proposal of the same turn this request revises.
	RevisesStepID int64 `json:"revises_step_id,omitempty"`
}

// ApprovalResolveRequest is a signed-in decider's verdict: approved, rejected, or edited.
// An edited verdict carries RevisedArguments, the JSON object the tool call runs with instead.
type ApprovalResolveRequest struct {
	RequestID        string          `json:"request_id"`
	ExecutionStepID  int64           `json:"execution_step_id"`
	Verdict          string          `json:"verdict"`
	ProposedDigest   string          `json:"proposed_digest"`
	Reason           string          `json:"reason,omitempty"`
	RevisedArguments json.RawMessage `json:"revised_arguments,omitempty"`
}

// ApprovalLinkRequest redeems a single-use decision link; the token names the person.
type ApprovalLinkRequest struct {
	Token           string `json:"token"`
	TenantID        int64  `json:"tenant_id"`
	RequestID       string `json:"request_id"`
	ExecutionStepID int64  `json:"execution_step_id"`
	Verdict         string `json:"verdict"`
	Reason          string `json:"reason,omitempty"`
}
