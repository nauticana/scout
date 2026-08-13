package domain

import "time"

// TenantContext identifies the tenant and its scheduling class.
type TenantContext struct {
	TenantID          int64
	PriorityClass     string
	DedicatedCapacity bool
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

// Usage records model, tool, and cost consumption.
type Usage struct {
	InputTokens    int64
	OutputTokens   int64
	ToolCalls      int
	CostMinorUnits int64
	Currency       string
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
