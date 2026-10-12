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

	ConfirmationsPath         = ApprovalBasePath + "confirmations"
	ConfirmationDecidePath    = ApprovalBasePath + "confirmation/decide"
	ConfirmationWithdrawPath  = ApprovalBasePath + "confirmation/withdraw"
	ConfirmationReconcilePath = ApprovalBasePath + "confirmation/reconcile"
)

// ApprovalRequest is one decision a reviewer owes; evidence stays behind its own authorization.
type ApprovalRequest struct {
	RequestID       string     `json:"request_id"`
	ExecutionStepID int64      `json:"execution_step_id"`
	ConversationID  string     `json:"conversation_id"`
	ScopeID         string     `json:"scope_id,omitempty"`
	RuleID          string     `json:"rule_id"`
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
	// RuleSource is why the call needs a person: autonomy, policy, or guardrail.
	RuleSource string `json:"rule_source"`
	// RouteID, RouteStep, and ApproverReference are the routed authority the request is assigned under.
	RouteID           string             `json:"route_id,omitempty"`
	RouteStep         int                `json:"route_step"`
	ApproverReference *ApproverReference `json:"approver_reference,omitempty"`
}

// ApprovalResolveRequest is a signed-in decider's verdict, an approval_status code: A approved,
// X rejected, or E edited.
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

// Confirmation is one MCP tool call awaiting or past a person's confirmation, with what the
// signed-in person may do now. Its stored payload stays server-side; preview is what a person reads.
type Confirmation struct {
	TenantID         int64           `json:"tenant_id"`
	ID               int64           `json:"id"`
	Maker            PrincipalRef    `json:"maker"`
	ClientRef        string          `json:"client_ref,omitempty"`
	Tool             string          `json:"tool"`
	PayloadDigest    string          `json:"payload_digest"`
	Preview          json.RawMessage `json:"preview,omitempty"`
	Channel          string          `json:"channel"`
	Status           string          `json:"status"`
	ApprovalRequired bool            `json:"approval_required"`
	Expired          bool            `json:"expired"`
	ExpiresAt        time.Time       `json:"expires_at"`
	CreatedAt        time.Time       `json:"created_at"`
	DecidedAt        *time.Time      `json:"decided_at,omitempty"`
	Decider          *PrincipalRef   `json:"decider,omitempty"`
	DecisionNote     string          `json:"decision_note,omitempty"`
	Error            string          `json:"error,omitempty"`
	CompletedAt      *time.Time      `json:"completed_at,omitempty"`
	Reconciler       *PrincipalRef   `json:"reconciler,omitempty"`
	ReconcileNote    string          `json:"reconcile_note,omitempty"`
	CanDecide        bool            `json:"can_decide"`
	CanWithdraw      bool            `json:"can_withdraw"`
	CanReconcile     bool            `json:"can_reconcile"`
}

// ConfirmationKey names one confirmation.
type ConfirmationKey struct {
	TenantID int64 `json:"tenant_id"`
	ID       int64 `json:"id"`
}

// ConfirmationDecideRequest approves or declines a pending confirmation.
type ConfirmationDecideRequest struct {
	ConfirmationKey
	Approve bool   `json:"approve"`
	Note    string `json:"note,omitempty"`
}

// ConfirmationReconcileRequest records the verified outcome of an unknown confirmation: an
// mcp_confirmation_status code, D executed or F failed.
type ConfirmationReconcileRequest struct {
	ConfirmationKey
	Outcome string `json:"outcome"`
	Note    string `json:"note,omitempty"`
}
