package domain

import "time"

// TenantContext identifies the tenant and its scheduling class.
type TenantContext struct {
	TenantID          int64  `json:"tenant_id"`
	PriorityClass     string `json:"priority_class,omitempty"`
	DedicatedCapacity bool   `json:"dedicated_capacity,omitempty"`
	// Tier is the bounded commercial class used as a metric label; never the tenant id.
	Tier string `json:"tier,omitempty"`
	// Region is the tenant's home residency region.
	Region string `json:"region,omitempty"`
	// ScopeID places the work in the tenant's scope tree; empty means the tenant root.
	ScopeID string `json:"scope_id,omitempty"`
}

// TurnAdmissionPolicy states what a new prompt does to a running turn.
type TurnAdmissionPolicy string

const (
	AdmissionQueue            TurnAdmissionPolicy = "queue"
	AdmissionCancelAndReplace TurnAdmissionPolicy = "cancel_replace"
)

// TenantRuntimePolicy defines hard limits for one tenant turn.
type TenantRuntimePolicy struct {
	PriorityClass     string
	CapacityClass     string
	MaxSteps          int
	MaxTokens         int64
	MaxCostMinorUnits int64
	CostCurrency      string
	TurnTimeout       time.Duration
	MidTurnPolicy     TurnAdmissionPolicy
}

// RuntimePolicyVersion names one immutable TenantRuntimePolicy. MidTurnPolicy is not versioned.
type RuntimePolicyVersion struct {
	Version string
	Policy  TenantRuntimePolicy
}

// Usage records model, tool, and cost consumption.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	ToolCalls    int
	// SearchQueries counts the provider-native web searches a grounded call ran.
	SearchQueries  int64
	CostMinorUnits int64
	Currency       string
	// Models is the share of the totals each model performed; the rest no model performed.
	Models []ModelSpend
}

// UsageAttribution names who spent the usage and where, so cost is reportable
// per agent and per organizational scope, not only per tenant.
type UsageAttribution struct {
	Principal PrincipalRef
	ScopeID   string
	// OnBehalfOf is the person the spending principal acted for; WorkRef the product work it served.
	OnBehalfOf PrincipalRef
	WorkRef    string
}

// UsageLine is settled usage of one agent release for one person, work item, and model
// in one currency; a zero Model is usage no model performed. Cost is in minor units of
// Currency, which has Exponent decimal places. Turns counts the turns whose model work
// the line holds, or on a zero-Model line the turns without model work, so a turn that
// used two models counts on both lines.
type UsageLine struct {
	Release        string
	Model          ModelReference
	OnBehalfOf     PrincipalRef
	WorkRef        string
	Currency       string
	Exponent       int
	Turns          int64
	InputTokens    int64
	OutputTokens   int64
	ToolCalls      int64
	CostMinorUnits int64
}

// FeedbackSummary counts the ratings one release received; Counts[r-1] holds rating r.
// Ratings are never reported per person.
type FeedbackSummary struct {
	Release string
	Counts  [5]int64
}

// BudgetRequest asks for a hold on one request's tokens and cost. Principal is who spends:
// empty or the turn's conversation agent, whose release budget the hold is charged to.
type BudgetRequest struct {
	TenantID       int64
	RequestID      string
	Principal      PrincipalRef
	Tokens         int64
	CostMinorUnits int64
	Currency       string
	// TurnGoverned says the caller stops the work at the release's per-turn ceiling, so a
	// per-turn budget clamps the grant instead of refusing a larger estimate.
	TurnGoverned bool
}

// BudgetReservation represents tokens and cost reserved for an operation.
type BudgetReservation struct {
	TenantID              int64
	ReservationID         string
	RequestID             string
	Attempt               int64
	GrantedTokens         int64
	GrantedCostMinorUnits int64
	Currency              string
	ExpiresAt             time.Time
}

// BudgetLimits is one tenant's rolling-window token and cost budget.
type BudgetLimits struct {
	WindowTokens         int64
	WindowCostMinorUnits int64
	Currency             string
	Window               time.Duration
}

// TurnBudget allocates a request deadline across stages; generation is
// reserved first and optional stages receive what remains.
type TurnBudget struct {
	Deadline    time.Time
	Total       time.Duration
	Generation  time.Duration
	Embedding   time.Duration
	Retrieval   time.Duration
	Rerank      time.Duration
	PromptBuild time.Duration
	Guardrail   time.Duration
}

// StageLatencyEstimate is the predicted cost of each budgeted stage for one request.
type StageLatencyEstimate struct {
	Admission     time.Duration
	Embedding     time.Duration
	Retrieval     time.Duration
	Rerank        time.Duration
	PromptBuild   time.Duration
	Guardrail     time.Duration
	MinGeneration time.Duration
	Generation    time.Duration
}
