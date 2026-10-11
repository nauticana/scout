package domain

import "time"

// MaxExecutionLimit is the largest value any field of a limits binding accepts.
const MaxExecutionLimit = 1<<31 - 1

// ExecutionLimits are the execution ceilings frozen into an agent's release by its limits
// binding. A zero field is not limited at the agent's scope and leaves the tenant runtime
// policy and the executor ceilings in force; a positive field only lowers them.
type ExecutionLimits struct {
	// TurnsPerMinute bounds the turns the agent opens in any minute, by the database clock.
	TurnsPerMinute int
	// MaxSteps lowers the tenant runtime policy's steps per turn.
	MaxSteps int
	// MaxToolCalls lowers the governed tool calls one tool-loop step may make.
	MaxToolCalls int
	// MaxDuration lowers the tenant runtime policy's turn timeout and each tool-loop step's deadline.
	MaxDuration time.Duration
}

// AgentLimitsReport is the limits and budget frozen into an agent's stable release, with
// the agent's current use of them.
type AgentLimitsReport struct {
	AgentID string
	Release string
	Limits  ExecutionLimits
	// TurnsLastMinute counts the agent's turns opened in the last minute by the database clock.
	TurnsLastMinute int64
	// Budget is nil when the release binds none.
	Budget *ReleaseBudget
	// BudgetProblem names why a bound budget cannot be enforced; Spend is then nil.
	BudgetProblem BudgetProblem
	// Spend is the spend of the budget's current period; nil for a per-turn budget or when the budget has a problem.
	Spend *BudgetSpend
}

// BudgetSpend is an agent's use of its budget's current period [PeriodStart, PeriodEnd) by the
// database clock: usage settled in the period and holds still live. Cost counts only Currency,
// whose minor units have Exponent decimal places.
type BudgetSpend struct {
	PeriodStart        time.Time
	PeriodEnd          time.Time
	Tokens             int64
	CostMinorUnits     int64
	HeldTokens         int64
	HeldCostMinorUnits int64
	Currency           string
	Exponent           int
}
