package domain

import "time"

// ApprovalStatus is the lifecycle of one runtime approval request.
type ApprovalStatus string

const (
	ApprovalStatusPending  ApprovalStatus = "pending"
	ApprovalStatusApproved ApprovalStatus = "approved"
	ApprovalStatusRejected ApprovalStatus = "rejected"
	// ApprovalStatusEdited rejects the original proposal; a changed action must
	// enter as a new request with its own digest before it can execute.
	ApprovalStatusEdited    ApprovalStatus = "edited"
	ApprovalStatusExpired   ApprovalStatus = "expired"
	ApprovalStatusEscalated ApprovalStatus = "escalated"
	ApprovalStatusWithdrawn ApprovalStatus = "withdrawn"
)

// OutputClass states what an actionable output is, so a reviewer is never shown
// a completed action as if it were a proposal.
type OutputClass string

const (
	OutputAdvice          OutputClass = "advice"
	OutputDraft           OutputClass = "draft"
	OutputApprovalRequest OutputClass = "approval_request"
	OutputCompletedAction OutputClass = "completed_action"
)

// RiskTier orders approval requests for a reviewer's attention.
type RiskTier string

const (
	RiskLow      RiskTier = "low"
	RiskMedium   RiskTier = "medium"
	RiskHigh     RiskTier = "high"
	RiskCritical RiskTier = "critical"
)

// ApprovalRequest is one durable decision a human owes. ProposedDigest binds the
// verdict to the exact action, so approving a changed action is impossible.
type ApprovalRequest struct {
	ID              int64
	TenantID        int64
	RequestID       string
	ConversationID  string
	ExecutionStepID int64
	Principal       PrincipalRef
	// Approver is the principal expected to decide; empty routes by scope.
	Approver       PrincipalRef
	ScopeID        string
	RuleID         string
	Action         string
	Resource       string
	Class          OutputClass
	RiskTier       RiskTier
	Summary        string
	Evidence       ObjectRef
	ProposedDigest string
	Status         ApprovalStatus
	DeadlineAt     time.Time
	CreatedAt      time.Time
	ResolvedAt     time.Time
}

// ApprovalVerdict resolves one request. Decider is the principal that acted and
// Authority is the grant it used, so a delegated approval is provable.
type ApprovalVerdict struct {
	RequestKey     ApprovalKey
	Status         ApprovalStatus
	Decider        PrincipalRef
	Authority      AuthorityRef
	ProposedDigest string
	Reason         string
	DecidedAt      time.Time
}

// ApprovalKey identifies one request without its payload.
type ApprovalKey struct {
	TenantID        int64
	RequestID       string
	ExecutionStepID int64
}

// ApprovalFilter selects pending work for one reviewer.
type ApprovalFilter struct {
	TenantID  int64
	Approver  PrincipalRef
	ScopeIDs  []string
	MinRisk   RiskTier
	DueBefore time.Time
	Limit     int
}

// EscalationStep is what happens when a deadline passes with no verdict.
type EscalationStep struct {
	// Backup receives the request; an empty backup abandons it instead.
	Backup PrincipalRef
	// Extension is the new deadline granted to the backup.
	Extension time.Duration
	Reason    string
}

// Notification is one outbound message about work a principal owes.
type Notification struct {
	TenantID  int64
	Recipient PrincipalRef
	Subject   string
	// Reference points at the item; a notification never carries evidence content.
	Reference string
	RiskTier  RiskTier
	DueAt     time.Time
}
