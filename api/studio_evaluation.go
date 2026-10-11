package api

import "time"

const (
	StudioCasesPath       = StudioBasePath + "cases"
	StudioCasesFreezePath = StudioBasePath + "cases/freeze"
	StudioEvaluatePath    = StudioBasePath + "evaluate"
	StudioEvaluationPath  = StudioBasePath + "evaluation"
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
}

// AgentEvaluationPage is GET evaluation: the run and one page of results in descending
// case id; next_before continues the page and is empty on the last one.
type AgentEvaluationPage struct {
	Run        AgentEvaluationRun `json:"run"`
	Results    []AgentCaseResult  `json:"results"`
	NextBefore string             `json:"next_before,omitempty"`
}
