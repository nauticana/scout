package domain

import "time"

// TenantContext identifies the tenant and its scheduling class.
type TenantContext struct {
	TenantID          int64
	PriorityClass     string
	DedicatedCapacity bool
}

// TenantRuntimePolicy defines hard limits for one tenant turn.
type TenantRuntimePolicy struct {
	PriorityClass     string
	CapacityClass     string
	MaxSteps          int
	MaxTokens         int64
	MaxCostMinorUnits int64
	CostCurrency      string
	TurnTimeout       time.Duration
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
	ReservationID         string
	GrantedTokens         int64
	GrantedCostMinorUnits int64
	Currency              string
}
