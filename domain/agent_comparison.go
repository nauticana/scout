package domain

import "time"

// EvaluationArm is one pinned arm of an agent evaluation: its subject, the frozen
// definition and settings of a draft, and the recorded outputs of a baseline by example id.
type EvaluationArm struct {
	Subject       AgentEvaluationSubject
	Draft         *AgentDefinition
	DraftSettings []EffectiveResource
	Outputs       map[string]ObjectRef
}

// CompareSubject names one arm of a comparison: the current draft, optionally frozen on
// another text model of the catalog, one release, or one recorded human baseline.
type CompareSubject struct {
	Draft bool
	// TextModel is the catalog model id a draft is frozen on in place of its own; empty keeps its own.
	TextModel  string
	Release    string
	BaselineID int64
}

// AgentCompareRequest asks for a golden set version to be run against two subjects and
// judged pairwise. A is the reference arm and the only one that may be a human baseline.
type AgentCompareRequest struct {
	AgentID    string
	SetVersion int64
	A, B       CompareSubject
}

// ComparisonVerdict is the arm a pairwise judgment preferred; arm a takes the baseline role
// of the run and arm b the candidate role.
type ComparisonVerdict string

const (
	ComparisonPrefersA ComparisonVerdict = "baseline"
	ComparisonPrefersB ComparisonVerdict = "candidate"
	ComparisonTie      ComparisonVerdict = "tie"
)

// CaseComparison is one example's pairwise outcome as a worker records it. Verdict is empty
// when judging failed, and Reason then says why. Evaluator is empty when no judge was
// needed because an arm ended in a failed state.
type CaseComparison struct {
	ExampleID      string
	A, B           EvaluationResult
	Verdict        ComparisonVerdict
	Confidence     float64
	Evaluator      EvaluatorVersion
	InputDigest    string
	Rationale      string
	EvaluatorUsage Usage
	Reason         string
}

// AgentCaseComparison is one example's pairwise outcome as the reading scope may see it;
// EvaluatorUsage is in the currency of Exponent.
type AgentCaseComparison struct {
	ExampleID      string
	Verdict        ComparisonVerdict
	Confidence     float64
	Evaluator      EvaluatorVersion
	Rationale      string
	Reason         string
	EvaluatorUsage Usage
	Exponent       int
	A, B           AgentCaseResult
}

// ComparisonTally counts a comparison's visible examples by verdict; Failed counts examples
// whose judging failed.
type ComparisonTally struct {
	A, B, Ties, Failed int
}

// ModelChange is one modality whose model differs between the release and the draft.
type ModelChange struct {
	Modality string
	Before   *ModelReference
	After    *ModelReference
}

// ComparisonDiff is what a compared draft changes against a compared release: models, compiled
// prompt sections, and effective settings other than prompt sections.
type ComparisonDiff struct {
	Release        string
	Models         []ModelChange
	PromptSections []PromptSectionChange
	Settings       []SettingChange
}

// AgentComparisonPage is one page of a comparison's examples in descending example id order;
// Diff is set when one arm is a draft and the other a release.
type AgentComparisonPage struct {
	Run        AgentEvaluationRun
	Tally      ComparisonTally
	Diff       *ComparisonDiff
	Cases      []AgentCaseComparison
	NextBefore string
}

// EvaluationBaseline is an immutable set of human reference outputs for one golden set
// version, one per example visible in Scope when it was recorded. ContentDigest digests the
// example ids with their output digests.
type EvaluationBaseline struct {
	BaselineID    int64
	TenantID      int64
	GoldenSetID   string
	SetVersion    int64
	Name          string
	Scope         GoldenScope
	ExampleCount  int
	ContentDigest string
	RecordedBy    PrincipalRef
	RecordedAt    time.Time
}

// BaselineOutput is the human reference answer to one case of a golden set version.
type BaselineOutput struct {
	CaseID string
	Output []byte
}

// BaselineRecord asks for a named baseline of the agent's golden set at SetVersion.
type BaselineRecord struct {
	AgentID    string
	SetVersion int64
	Name       string
	Outputs    []BaselineOutput
}
