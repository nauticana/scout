package api

import "time"

const (
	// StudioComparePath queues a comparison (POST) or pages one (GET ?run_id=&before=&limit=).
	StudioComparePath = StudioBasePath + "compare"
	// StudioBaselinesPath lists a golden set version's human baselines: GET ?agent_name=&set_version=.
	StudioBaselinesPath = StudioBasePath + "baselines"
	StudioBaselinePath  = StudioBasePath + "baseline"
)

// CompareSubject is one arm of a comparison: the draft (draft true, frozen on model when set),
// one release, or, for arm a only, one recorded baseline; exactly one of them.
type CompareSubject struct {
	Draft      bool   `json:"draft,omitempty"`
	Model      string `json:"model,omitempty"`
	Release    string `json:"release,omitempty"`
	BaselineID int64  `json:"baseline_id,omitempty"`
}

// AgentCompareRequest is POST compare: set_version is the frozen golden set version whose
// cases both arms answer; a is the reference and b the challenger.
type AgentCompareRequest struct {
	AgentName  string         `json:"agent_name"`
	SetVersion int64          `json:"set_version"`
	A          CompareSubject `json:"a"`
	B          CompareSubject `json:"b"`
}

// ComparedSubject is one pinned arm: subject is D (draft), R (release), or B (baseline); a draft frozen on
// another text model names it in provider and model.
type ComparedSubject struct {
	Subject          string `json:"subject"`
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	Release          string `json:"release,omitempty"`
	BaselineID       int64  `json:"baseline_id,omitempty"`
	DefinitionDigest string `json:"definition_digest,omitempty"`
}

// AgentComparisonRun is one queued comparison and its settled totals, both arms and the judge.
type AgentComparisonRun struct {
	RunID         int64           `json:"run_id"`
	AgentName     string          `json:"agent_name"`
	GoldenSetID   string          `json:"golden_set_id"`
	SetVersion    int64           `json:"set_version"`
	A             ComparedSubject `json:"a"`
	B             ComparedSubject `json:"b"`
	Status        string          `json:"status"`
	QueuedAt      time.Time       `json:"queued_at"`
	StartedAt     *time.Time      `json:"started_at,omitempty"`
	CompletedAt   *time.Time      `json:"completed_at,omitempty"`
	Samples       int             `json:"samples"`
	HiddenResults int             `json:"hidden_results"`
	Cost          EvaluationCost  `json:"cost"`
	Reason        string          `json:"reason,omitempty"`
}

// ComparisonTally counts the compared cases by verdict; failed counts cases whose judging failed.
type ComparisonTally struct {
	A      int `json:"a"`
	B      int `json:"b"`
	Tie    int `json:"tie"`
	Failed int `json:"failed"`
}

// ComparedAnswer is one arm's answer to a case with its digest, judge scores, latency, and cost;
// an arm that ended in a failed state has no answer, and output_omitted marks an answer left
// out to keep the page within its byte budget.
type ComparedAnswer struct {
	Output        string            `json:"output,omitempty"`
	OutputOmitted bool              `json:"output_omitted,omitempty"`
	OutputDigest  string            `json:"output_digest,omitempty"`
	Scores        []EvaluationScore `json:"scores"`
	LatencyMs     int64             `json:"latency_ms"`
	Cost          EvaluationCost    `json:"cost"`
}

// CaseComparison is one case's verdict code (evaluation_comparison_verdict), absent when judging failed and reason
// says why. evaluator is absent when an arm ended in a failed state and no judge was asked;
// evaluator_cost is what the judge spent.
type CaseComparison struct {
	CaseID        string            `json:"case_id"`
	Verdict       string            `json:"verdict,omitempty"`
	Confidence    float64           `json:"confidence"`
	Evaluator     *EvaluatorVersion `json:"evaluator,omitempty"`
	Rationale     string            `json:"rationale,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	EvaluatorCost EvaluationCost    `json:"evaluator_cost"`
	A             ComparedAnswer    `json:"a"`
	B             ComparedAnswer    `json:"b"`
}

// ModelChange is one modality whose model id differs between the release and the draft.
type ModelChange struct {
	Modality string `json:"modality"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
}

// ComparisonDiff is what the compared draft changes against the compared release.
type ComparisonDiff struct {
	Release        string                `json:"release"`
	Models         []ModelChange         `json:"models"`
	PromptSections []PromptSectionChange `json:"prompt_sections"`
	Settings       []SettingChange       `json:"settings"`
}

// AgentComparisonPage is GET compare: the run, its tally, the diff when it compares the draft
// with a release, and one page of cases in descending case id continued by next_before.
type AgentComparisonPage struct {
	Run        AgentComparisonRun `json:"run"`
	Tally      ComparisonTally    `json:"tally"`
	Diff       *ComparisonDiff    `json:"diff,omitempty"`
	Cases      []CaseComparison   `json:"cases"`
	NextBefore string             `json:"next_before,omitempty"`
}

// BaselineOutput is the human reference answer to one case.
type BaselineOutput struct {
	CaseID string `json:"case_id"`
	Output string `json:"output"`
}

// BaselineRecordRequest is POST baseline: one output for every case of the frozen set version.
type BaselineRecordRequest struct {
	AgentName  string           `json:"agent_name"`
	SetVersion int64            `json:"set_version"`
	Name       string           `json:"name"`
	Outputs    []BaselineOutput `json:"outputs"`
}

// EvaluationBaseline is one recorded human baseline of a golden set version.
type EvaluationBaseline struct {
	BaselineID    int64     `json:"baseline_id"`
	GoldenSetID   string    `json:"golden_set_id"`
	SetVersion    int64     `json:"set_version"`
	Name          string    `json:"name"`
	ExampleCount  int       `json:"example_count"`
	ContentDigest string    `json:"content_digest"`
	RecordedAt    time.Time `json:"recorded_at"`
}
