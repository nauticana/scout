package domain

import "time"

// ApprovalStatus is the lifecycle of one runtime approval request.
type ApprovalStatus string

const (
	ApprovalStatusPending  ApprovalStatus = "P"
	ApprovalStatusApproved ApprovalStatus = "A"
	ApprovalStatusRejected ApprovalStatus = "X"
	// ApprovalStatusEdited closes the original proposal with revised arguments; the
	// revision enters as a new request with its own digest and runs in its place.
	ApprovalStatusEdited    ApprovalStatus = "E"
	ApprovalStatusExpired   ApprovalStatus = "T"
	ApprovalStatusEscalated ApprovalStatus = "S"
	ApprovalStatusWithdrawn ApprovalStatus = "W"
)

// OutputClass states what an actionable output is, so a reviewer is never shown
// a completed action as if it were a proposal.
type OutputClass string

const (
	OutputAdvice          OutputClass = "A"
	OutputDraft           OutputClass = "D"
	OutputApprovalRequest OutputClass = "R"
	OutputCompletedAction OutputClass = "C"
)

// Valid reports whether the class is a seeded agent_output_class code.
func (class OutputClass) Valid() bool {
	switch class {
	case OutputAdvice, OutputDraft, OutputApprovalRequest, OutputCompletedAction:
		return true
	default:
		return false
	}
}

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
	// NotificationEventID is the keel outbox event that notifies the first approver; zero when none was queued.
	NotificationEventID int64
	// RevisesStepID is the step of the edited proposal this request revises, in the same turn; zero for an original.
	RevisesStepID int64
	// ReleaseVersion is the agent release whose frozen routing applies; empty for other principals.
	ReleaseVersion string
	// Requester is the person the turn acts for; nobody approves their own request.
	Requester PrincipalRef
	// Source is why the call needs a person; every request records one, with its RuleID.
	Source ApprovalSource
	// Route is the routed authority; an empty RouteID is a request routed by scope or a product router.
	Route ApprovalRouteAuthority
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
	// RevisedArguments are an edited verdict's replacement tool-call arguments, a JSON object.
	RevisedArguments []byte
	// Revision is where the revised arguments are archived; the store keeps only this reference.
	Revision ObjectRef
	// Assignment is the assignment the decider was authorized against, set by the server from
	// the request it read; a request reassigned since refuses the verdict.
	Assignment ApprovalFence
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

// EscalationAction is what a sweep does with one overdue or vacant request.
type EscalationAction string

const (
	// EscalationReassign hands the request to Backup.
	EscalationReassign EscalationAction = "reassign"
	// EscalationVacate moves a routed request to a later step with nobody assigned yet.
	EscalationVacate EscalationAction = "vacate"
	// EscalationHold keeps the current assignment open with no deadline.
	EscalationHold EscalationAction = "hold"
	// EscalationWait leaves the request untouched for a later sweep.
	EscalationWait EscalationAction = "wait"
	// EscalationExpire closes the request as expired, a denial.
	EscalationExpire EscalationAction = "expire"
)

// EscalationStep is what happens when a deadline passes with no verdict, or a vacancy waits.
type EscalationStep struct {
	// Action is empty to infer it: a set Backup reassigns, otherwise the request expires.
	Action EscalationAction
	// Backup receives the request on a reassignment.
	Backup PrincipalRef
	// Extension is the new deadline from now; zero leaves the request with no deadline.
	Extension time.Duration
	// KeepDeadline fills a vacancy in place under the current deadline.
	KeepDeadline bool
	// Route is the routed authority after the step; an unrouted request leaves it empty.
	Route  ApprovalRouteAuthority
	Reason string
}

// Resolved returns the step's action, inferring it when empty.
func (s EscalationStep) Resolved() EscalationAction {
	switch {
	case s.Action != "":
		return s.Action
	case s.Backup.ID != "":
		return EscalationReassign
	default:
		return EscalationExpire
	}
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
