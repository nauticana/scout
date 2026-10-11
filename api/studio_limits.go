package api

import "time"

// StudioLimitsPath serves an agent's frozen execution limits and budget with current use.
const StudioLimitsPath = StudioBasePath + "limits"

// Budget periods: one turn, a trailing window, or a calendar day or month in a timezone.
const (
	BudgetPeriodTurn    = "turn"
	BudgetPeriodRolling = "rolling"
	BudgetPeriodDay     = "day"
	BudgetPeriodMonth   = "month"
)

// AgentLimits is the execution limits and budget frozen into an agent's stable release, with
// the agent's turns opened in the last minute and its spend in the budget's current period.
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

// AgentBudget is a release budget: token and cost ceilings over its period; cost is in minor
// units of currency with exponent decimal places. source is release (frozen with the release) or
// fallback (the deployment's budget for a release that binds none). problem names why the
// budget cannot be enforced, when it cannot.
type AgentBudget struct {
	Tokens         int64  `json:"tokens"`
	CostMinorUnits int64  `json:"cost_minor_units"`
	Currency       string `json:"currency,omitempty"`
	Exponent       *int   `json:"exponent,omitempty"`
	Period         string `json:"period"`
	Source         string `json:"source"`
	WindowSeconds  int64  `json:"window_seconds"`
	Timezone       string `json:"timezone,omitempty"`
	Problem        string `json:"problem,omitempty"`
}

// AgentBudgetSpend is the use in [period_start, period_end) by the database clock; a rolling
// window ends at the database's now. Settled usage and live holds are reported apart; cost
// counts only usage in the budget's currency, which has exponent decimal places.
type AgentBudgetSpend struct {
	PeriodStart        time.Time `json:"period_start"`
	PeriodEnd          time.Time `json:"period_end"`
	Tokens             int64     `json:"tokens"`
	CostMinorUnits     int64     `json:"cost_minor_units"`
	HeldTokens         int64     `json:"held_tokens"`
	HeldCostMinorUnits int64     `json:"held_cost_minor_units"`
	Currency           string    `json:"currency,omitempty"`
	Exponent           int       `json:"exponent"`
}
