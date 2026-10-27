package api

import (
	"encoding/json"
	"time"
)

const (
	StudioCasesPath       = StudioBasePath + "cases"
	StudioCasesFreezePath = StudioBasePath + "cases/freeze"
	StudioEvaluatePath    = StudioBasePath + "evaluate"
	StudioEvaluationPath  = StudioBasePath + "evaluation"

	StudioEvaluationTrajectoryPath = StudioBasePath + "evaluation/trajectory"
)

// GoldenCase is one saved test case of an agent's golden set.
type GoldenCase struct {
	CaseID           string    `json:"case_id"`
	Name             string    `json:"name"`
	Input            string    `json:"input"`
	Language         string    `json:"language,omitempty"`
	ExpectedBehavior string    `json:"expected_behavior,omitempty"`
	Rubric           string    `json:"rubric,omitempty"`
	RiskTier         string    `json:"risk_tier,omitempty"`
	ModifiedAt       time.Time `json:"modified_at,omitzero"`
	// ReviewItemID names the review item a promoted case came from; its input is withheld
	// (input_withheld) from a reader without REVIEW on that turn's agent.
	ReviewItemID  int64 `json:"review_item_id,omitempty"`
	InputWithheld bool  `json:"input_withheld,omitempty"`
	// Turns are the inputs of turns 2..n, each submitted once the previous turn completes.
	Turns []string `json:"turns,omitempty"`
	// Tools are the sandboxed responses tool calls get while the case replays.
	Tools []GoldenCaseTool `json:"tools,omitempty"`
	// RequiredEvidence and ControllingAuthority are the ids the case's decision needs to reach the
	// model; ExecutionExpected says whether a mutating tool call is expected, absent when unlabeled.
	RequiredEvidence     []string `json:"required_evidence,omitempty"`
	ControllingAuthority []string `json:"controlling_authority,omitempty"`
	ExecutionExpected    *bool    `json:"execution_expected,omitempty"`
}

// GoldenCaseTool answers every call of one tool: output is JSON; a failure (is_error) carries
// none and reaches the model as a failed tool call.
type GoldenCaseTool struct {
	ToolID  string          `json:"tool_id"`
	Output  json.RawMessage `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

// GoldenSetVersion is one frozen, immutable version of a golden set.
type GoldenSetVersion struct {
	SetVersion      int64     `json:"set_version"`
	DatasetRevision string    `json:"dataset_revision"`
	ExampleCount    int       `json:"example_count"`
	FrozenAt        time.Time `json:"frozen_at"`
}

// GoldenCaseSet is an agent's saved test cases; hidden gate cases are only counted.
type GoldenCaseSet struct {
	AgentName   string             `json:"agent_name"`
	GoldenSetID string             `json:"golden_set_id"`
	Revision    int64              `json:"revision"`
	Cases       []GoldenCase       `json:"cases"`
	HiddenCases int                `json:"hidden_cases"`
	Versions    []GoldenSetVersion `json:"versions"`
}

// GoldenCaseChange is POST cases: op associate takes golden_set_id and name, put takes
// case, remove takes case_id; put and remove name the set's current revision.
type GoldenCaseChange struct {
	AgentName        string     `json:"agent_name"`
	Op               string     `json:"op"`
	GoldenSetID      string     `json:"golden_set_id,omitempty"`
	Name             string     `json:"name,omitempty"`
	Case             GoldenCase `json:"case"`
	CaseID           string     `json:"case_id,omitempty"`
	ExpectedRevision int64      `json:"expected_revision"`
}

// GoldenCaseFreeze is POST cases/freeze.
type GoldenCaseFreeze struct {
	AgentName        string `json:"agent_name"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// GoldenCaseFrozen is the version a freeze created and the set after it.
type GoldenCaseFrozen struct {
	Version GoldenSetVersion `json:"version"`
	Set     GoldenCaseSet    `json:"set"`
}

// EvaluationSubject is the draft (draft true) or one release (release set), never both.
type EvaluationSubject struct {
	Draft   bool   `json:"draft,omitempty"`
	Release string `json:"release,omitempty"`
}

// AgentEvaluateRequest is POST evaluate.
type AgentEvaluateRequest struct {
	AgentName  string            `json:"agent_name"`
	SetVersion int64             `json:"set_version"`
	Subject    EvaluationSubject `json:"subject"`
}

// EvaluationCost is integer minor units of currency, which has exponent decimal places.
type EvaluationCost struct {
	InputTokens    int64  `json:"input_tokens"`
	OutputTokens   int64  `json:"output_tokens"`
	CostMinorUnits int64  `json:"cost_minor_units"`
	Currency       string `json:"currency,omitempty"`
	Exponent       int    `json:"exponent"`
}

// AgentEvaluationRun is one queued agent evaluation and its settled totals.
type AgentEvaluationRun struct {
	RunID            int64          `json:"run_id"`
	AgentName        string         `json:"agent_name"`
	GoldenSetID      string         `json:"golden_set_id"`
	SetVersion       int64          `json:"set_version"`
	Subject          string         `json:"subject"`
	Release          string         `json:"release,omitempty"`
	DefinitionDigest string         `json:"definition_digest"`
	Status           string         `json:"status"`
	QueuedAt         time.Time      `json:"queued_at"`
	StartedAt        *time.Time     `json:"started_at,omitempty"`
	CompletedAt      *time.Time     `json:"completed_at,omitempty"`
	Samples          int            `json:"samples"`
	HiddenResults    int            `json:"hidden_results"`
	Cost             EvaluationCost `json:"cost"`
	Reason           string         `json:"reason,omitempty"`
}

// EvaluatorVersion identifies the evaluator revision that produced a score.
type EvaluatorVersion struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// EvaluationScore is one metric value with its evaluator and rationale.
type EvaluationScore struct {
	Metric     string           `json:"metric"`
	Value      float64          `json:"value"`
	Confidence float64          `json:"confidence"`
	Evaluator  EvaluatorVersion `json:"evaluator"`
	Rationale  string           `json:"rationale,omitempty"`
	Critical   bool             `json:"critical"`
}

// AgentCaseResult is one example's scored outcome; cost is the subject's spend and
// evaluator_cost what model-based evaluators spent scoring it.
type AgentCaseResult struct {
	CaseID           string            `json:"case_id"`
	Scores           []EvaluationScore `json:"scores"`
	LatencyMs        int64             `json:"latency_ms"`
	Cost             EvaluationCost    `json:"cost"`
	EvaluatorCost    EvaluationCost    `json:"evaluator_cost"`
	NeedsHumanReview bool              `json:"needs_human_review"`
	Reason           string            `json:"reason,omitempty"`
	// HasTrajectory marks a case whose trajectory GET evaluation/trajectory returns.
	HasTrajectory bool `json:"has_trajectory,omitempty"`
}

// TrajectoryUsage is one trajectory event's spend; cost is in minor units of currency.
type TrajectoryUsage struct {
	InputTokens    int64  `json:"input_tokens,omitempty"`
	OutputTokens   int64  `json:"output_tokens,omitempty"`
	ToolCalls      int    `json:"tool_calls,omitempty"`
	SearchQueries  int64  `json:"search_queries,omitempty"`
	CostMinorUnits int64  `json:"cost_minor_units,omitempty"`
	Currency       string `json:"currency,omitempty"`
}

// TrajectoryEvent is one sequenced event: kind is token, tool_call, observation, state, or
// policy; a state event named turn marks where each turn after the first starts.
type TrajectoryEvent struct {
	Sequence  int64            `json:"sequence"`
	Kind      string           `json:"kind"`
	Name      string           `json:"name,omitempty"`
	Payload   json.RawMessage  `json:"payload,omitempty"`
	OffsetMs  int64            `json:"offset_ms"`
	Failed    bool             `json:"failed,omitempty"`
	Recovered bool             `json:"recovered,omitempty"`
	Usage     *TrajectoryUsage `json:"usage,omitempty"`
}

// CaseTrajectory is GET evaluation/trajectory?run_id=&case_id=&arm=: one case's trajectory as
// arm b (the default) or arm a of a comparison ran it.
type CaseTrajectory struct {
	RunID  int64             `json:"run_id"`
	CaseID string            `json:"case_id"`
	Arm    string            `json:"arm"`
	Events []TrajectoryEvent `json:"events"`
}

// AgentEvaluationPage is GET evaluation: the run and one page of results in descending
// case id; next_before continues the page and is empty on the last one.
type AgentEvaluationPage struct {
	Run        AgentEvaluationRun `json:"run"`
	Results    []AgentCaseResult  `json:"results"`
	NextBefore string             `json:"next_before,omitempty"`
}
