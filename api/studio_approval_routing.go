package api

// StudioApprovalRoutingPath serves an agent's effective approval routing and the asks its draft can make.
const StudioApprovalRoutingPath = StudioBasePath + "approval-routing"

// AgentApprovalRouting is an agent's effective approval routes in precedence order, every
// approval its draft can ask for with the route serving it, and what publication would refuse.
type AgentApprovalRouting struct {
	AgentName        string                 `json:"agent_name"`
	Routes           []ApprovalRouteView    `json:"routes"`
	Coverage         []ApprovalCoverageView `json:"coverage"`
	ResolverComposed bool                   `json:"resolver_composed"`
	Problems         []string               `json:"problems,omitempty"`
}

// ApprovalRouteView is one effective route; scope_id is the widest scope declaring it, and
// inherited marks a route the agent's own scope did not declare first.
type ApprovalRouteView struct {
	ID              string                   `json:"id"`
	Match           ApprovalRouteMatch       `json:"match"`
	Approver        ApproverReference        `json:"approver"`
	DeadlineSeconds int64                    `json:"deadline_seconds"`
	Escalation      []ApprovalEscalationView `json:"escalation,omitempty"`
	OnExhausted     string                   `json:"on_exhausted"`
	ScopeID         string                   `json:"scope_id"`
	Inherited       bool                     `json:"inherited"`
}

// ApprovalRouteMatch selects the asks a route serves; an empty field matches any.
type ApprovalRouteMatch struct {
	Tools       []string `json:"tools,omitempty"`
	Sources     []string `json:"sources,omitempty"`
	MinRiskTier string   `json:"min_risk_tier,omitempty"`
}

// ApproverReference names a role or position by the product's opaque reference.
type ApproverReference struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// ApprovalEscalationView is one escalation step; without an approver it extends the deadline.
type ApprovalEscalationView struct {
	Approver         *ApproverReference `json:"approver,omitempty"`
	ExtensionSeconds int64              `json:"extension_seconds"`
}

// ApprovalCoverageView is one approval the draft can ask for; route_id is absent when no route serves it.
type ApprovalCoverageView struct {
	ToolID   string `json:"tool_id"`
	Source   string `json:"source"`
	RiskTier string `json:"risk_tier"`
	RouteID  string `json:"route_id,omitempty"`
}
