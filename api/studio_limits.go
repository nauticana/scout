package api

import "time"

// StudioLimitsPath serves an agent's frozen execution limits and budget with current use.
const StudioLimitsPath = StudioBasePath + "limits"

// AgentLimits is the execution limits and budget frozen into an agent's stable release, with
// the agent's turns opened in the last minute and its spend in the budget's rolling window.
type AgentLimits struct {
	AgentName       string               `json:"agent_name"`
	Version         string               `json:"version"`
	Limits          AgentExecutionLimits `json:"limits"`
	TurnsLastMinute int64                `json:"turns_last_minute"`
	Budget          *AgentBudget         `json:"budget,omitempty"`
	Spend           *AgentBudgetSpend    `json:"spend,omitempty"`
}

// AgentExecutionLimits is the limits binding value; zero is not limited at the agent's scope.
type AgentExecutionLimits struct {
	TurnsPerMinute     int   `json:"turns_per_minute"`
	MaxSteps           int   `json:"max_steps"`
	MaxToolCalls       int   `json:"max_tool_calls"`
	MaxDurationSeconds int64 `json:"max_duration_seconds"`
}

// AgentBudget is a release budget: token and cost ceilings over a rolling window of
// window_seconds (0 when the composition sets none); cost is in minor units of currency.
type AgentBudget struct {
	Tokens         int64  `json:"tokens"`
	CostMinorUnits int64  `json:"cost_minor_units"`
	Currency       string `json:"currency,omitempty"`
	WindowSeconds  int64  `json:"window_seconds"`
}

// AgentBudgetSpend is the settled use in the window [period_start, period_end); cost counts
// only usage in the budget's currency, which has exponent decimal places.
type AgentBudgetSpend struct {
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	Tokens         int64     `json:"tokens"`
	CostMinorUnits int64     `json:"cost_minor_units"`
	Currency       string    `json:"currency,omitempty"`
	Exponent       int       `json:"exponent"`
}
