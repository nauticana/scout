package domain

import "time"

// ApprovalSource is why a tool call needs a person: the agent's operating mode, a policy
// obligation, or a guardrail rule.
type ApprovalSource string

const (
	ApprovalSourceAutonomy  ApprovalSource = "autonomy"
	ApprovalSourcePolicy    ApprovalSource = "policy"
	ApprovalSourceGuardrail ApprovalSource = "guardrail"
)

const (
	// DefaultApprovalRiskTier is the risk of an ask that names none.
	DefaultApprovalRiskTier = RiskHigh
	// MaxApprovalEscalationSteps bounds the escalation of one route.
	MaxApprovalEscalationSteps = 3
	// MaxApprovalRoutes bounds the effective routing of one agent.
	MaxApprovalRoutes = 64
	// MaxApprovalRoutesPerBinding bounds the routes of one approval_routing binding.
	MaxApprovalRoutesPerBinding = 32
	// MaxApprovalRouteToolPatterns bounds the tool patterns one route matches.
	MaxApprovalRouteToolPatterns = 16
	// MinApprovalRouteDurationSeconds and MaxApprovalRouteDurationSeconds bound one deadline or extension.
	MinApprovalRouteDurationSeconds = 60
	MaxApprovalRouteDurationSeconds = 30 * 24 * 60 * 60
	// MaxApprovalRouteWindowSeconds bounds a route's deadline plus all its extensions.
	MaxApprovalRouteWindowSeconds = 90 * 24 * 60 * 60
	// DefaultMaxApproverCandidates is the eligible principals one resolution considers by default.
	DefaultMaxApproverCandidates = 20
)

// ApproverReferenceKind says what an approver reference names. Scout never models either:
// the product resolves the reference to its current holders.
type ApproverReferenceKind string

const (
	ApproverRole     ApproverReferenceKind = "role"
	ApproverPosition ApproverReferenceKind = "position"
)

// ApproverReference names a role or position by the product's opaque reference.
type ApproverReference struct {
	Kind ApproverReferenceKind `json:"kind"`
	Ref  string                `json:"ref"`
}

// ApprovalRouteMatch selects the asks a route serves; an empty field matches any.
type ApprovalRouteMatch struct {
	// Tools are exact tool ids or prefixes ending in one trailing "*".
	Tools       []string         `json:"tools,omitempty"`
	Sources     []ApprovalSource `json:"sources,omitempty"`
	MinRiskTier RiskTier         `json:"min_risk_tier,omitempty"`
}

// ApprovalEscalation is one step taken when a deadline passes: a step with an approver hands
// the work to that role or position, one without extends the deadline under the current one.
type ApprovalEscalation struct {
	Approver         *ApproverReference `json:"approver,omitempty"`
	ExtensionSeconds int64              `json:"extension_seconds"`
}

// ApprovalExhaustion is what happens when the last escalation step's deadline passes.
type ApprovalExhaustion string

const (
	// ApprovalExhaustExpire closes the request as expired, a denial that fails the turn.
	ApprovalExhaustExpire ApprovalExhaustion = "expire"
	// ApprovalExhaustHold keeps the request open with no deadline; the work stays suspended.
	ApprovalExhaustHold ApprovalExhaustion = "hold"
)

// ApprovalRoute is who decides the asks it matches, by when, and how overdue work escalates.
type ApprovalRoute struct {
	ID              string               `json:"id"`
	Match           ApprovalRouteMatch   `json:"match"`
	Approver        ApproverReference    `json:"approver"`
	DeadlineSeconds int64                `json:"deadline_seconds"`
	Escalation      []ApprovalEscalation `json:"escalation,omitempty"`
	OnExhausted     ApprovalExhaustion   `json:"on_exhausted"`
}

// ApprovalRouting is the approval_routing setting; the first matching route wins.
type ApprovalRouting struct {
	Routes []ApprovalRoute `json:"routes"`
}

// ApprovalAsk is why one call needs a person.
type ApprovalAsk struct {
	Source ApprovalSource
	RuleID string
	// RiskTier is empty for DefaultApprovalRiskTier.
	RiskTier RiskTier
}

// ApprovalRouteAuthority is the routed authority a request is assigned under: the route, the
// role or position at the current escalation step, and the resolver's evidence for the pick.
type ApprovalRouteAuthority struct {
	RouteID    string
	Reference  ApproverReference
	Step       int
	Resolution string
}

// ApprovalAssignment is a router's answer for one request. An empty Approver is a vacancy,
// allowed only under a route: nobody may decide until escalation fills it.
type ApprovalAssignment struct {
	Approver  PrincipalRef
	Authority ApprovalRouteAuthority
	// Deadline is how long the approver has; zero takes the gate's default.
	Deadline time.Duration
}

// ApproverQuery asks a product directory for the holders of a role or position.
type ApproverQuery struct {
	TenantID  int64
	Reference ApproverReference
	Request   ApprovalRequest
	Limit     int
}

// EligibleApprover is one holder of a role or position; Resolution is the product's evidence
// reference for why this principal holds it, never personal data beyond that reference.
type EligibleApprover struct {
	Principal  PrincipalRef
	Resolution string
}

// ApprovalFence is the assignment a verdict or reassignment was decided against; a request
// assigned since then refuses it.
type ApprovalFence struct {
	Step     int
	Approver PrincipalRef
}

// ApprovalCoverage is one ask a release can make and the route serving it; an empty RouteID
// is uncovered.
type ApprovalCoverage struct {
	ToolID   string
	Source   ApprovalSource
	RiskTier RiskTier
	RouteID  string
}

// ApprovalRouteOrigin is one effective route and the widest scope declaring it.
type ApprovalRouteOrigin struct {
	Route     ApprovalRoute
	ScopeID   string
	Inherited bool
}

// ApprovalRoutingView is an agent's effective routing, the asks its draft can make, and what
// publication would refuse.
type ApprovalRoutingView struct {
	AgentID          string
	Routes           []ApprovalRouteOrigin
	Coverage         []ApprovalCoverage
	ResolverComposed bool
	Problems         []string
}
