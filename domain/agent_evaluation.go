package domain

import (
	"encoding/json"
	"time"
)

// GoldenCase is one editable example in a golden set's working copy. Freezing the
// working copy snapshots every case into an immutable GoldenSetVersion of GoldenExamples.
type GoldenCase struct {
	TenantID    int64
	GoldenSetID string
	CaseID      string
	Name        string
	// Input is the turn input exactly as a conversation would submit it.
	Input            []byte
	Language         string
	ExpectedBehavior []byte
	RubricRef        string
	// RiskTier is an approval_risk_tier code; empty means low.
	RiskTier string
	// Hidden marks a gate case that dev-scope reads and writes never reach.
	Hidden     bool
	ModifiedBy PrincipalRef
	ModifiedAt time.Time
	// ReviewItemID names the production review item the case was promoted from, and
	// ReviewAgentID that item's agent; such an input is production content only REVIEW
	// holders on that agent read. InputWithheld marks an Input withheld from the reader.
	ReviewItemID  int64
	ReviewAgentID string
	InputWithheld bool
	// Script holds the case's follow-up turns and sandboxed tool responses; zero is unscripted.
	Script    CaseScript
	Assurance AssuranceLabels
}

// CaseScript is the scripted part of a golden case: the inputs of turns 2..n, each
// submitted once the previous turn completes, and the sandboxed response every call of a
// tool is answered with. Turn 1 is the case input. The zero script is a single-turn case
// whose tool calls no sandbox answers.
type CaseScript struct {
	Turns []CaseTurn `json:"turns,omitempty"`
	Tools []CaseTool `json:"tools,omitempty"`
}

// Scripted reports whether the script names a follow-up turn or a tool response.
func (script CaseScript) Scripted() bool { return len(script.Turns) > 0 || len(script.Tools) > 0 }

// CaseTurn is one follow-up turn's input exactly as a conversation would submit it.
type CaseTurn struct {
	Input string `json:"input"`
}

// CaseTool is the sandboxed response to every call of one tool: its JSON output, or a
// failure (IsError) that carries no output, since the model sees only a failure's class.
type CaseTool struct {
	ToolID  string          `json:"tool_id"`
	Output  json.RawMessage `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

// CaseTrajectory is the stored trajectory of one executed case of a run as one arm ran it.
type CaseTrajectory struct {
	RunID     int64
	ExampleID string
	Role      EvaluationRole
	Events    []TrajectoryEvent
}

// GoldenCaseSet is an agent's golden set as one scope sees it: the working copy,
// how many gate cases it withholds, and the newest frozen versions.
type GoldenCaseSet struct {
	AgentID     string
	GoldenSetID string
	// Revision is the working revision every change and freeze must name.
	Revision    int64
	Cases       []GoldenCase
	HiddenCases int
	Versions    []GoldenSetVersion
}

// GoldenSetAssociation points an agent at a tenant golden set. A new set is created
// under Name and owned by Owner; an existing set must already be owned by Owner.
// An empty Owner is a set no agent owns, which only a product associates.
type GoldenSetAssociation struct {
	TenantID    int64
	AgentID     string
	GoldenSetID string
	Name        string
	Owner       string
	By          PrincipalRef
}

// GoldenCaseOp is one change to an agent's golden set.
type GoldenCaseOp string

const (
	// GoldenCaseAssociate points the agent at a tenant golden set, creating it owned by the agent when new.
	GoldenCaseAssociate GoldenCaseOp = "associate"
	GoldenCasePut       GoldenCaseOp = "put"
	GoldenCaseRemove    GoldenCaseOp = "remove"
)

// GoldenCaseChange adds, replaces, or removes one case behind the set's working
// revision, or associates the agent with a golden set.
type GoldenCaseChange struct {
	AgentID          string
	Op               GoldenCaseOp
	GoldenSetID      string
	SetName          string
	Case             GoldenCase
	CaseID           string
	ExpectedRevision int64
}

// EvaluationSubjectKind names what an agent evaluation executes.
type EvaluationSubjectKind string

const (
	// SubjectDraft is the agent's current draft frozen exactly as Publish would freeze it, never persisted as a release.
	SubjectDraft   EvaluationSubjectKind = "D"
	SubjectRelease EvaluationSubjectKind = "R"
	// SubjectBaseline is a recorded human baseline: stored reference outputs, never executed.
	SubjectBaseline EvaluationSubjectKind = "B"
)

// AgentEvaluationSubject pins one arm of an agent evaluation: a frozen draft
// identified by its definition digest, a published release, or a recorded human baseline.
type AgentEvaluationSubject struct {
	Kind    EvaluationSubjectKind
	AgentID string
	// Version is the release a release subject runs; empty for a draft.
	Version string
	// DefinitionDigest is empty for a baseline.
	DefinitionDigest string
	// Model is the text model a draft subject was frozen on in place of its own; nil is its own.
	Model *ModelReference
	// BaselineID is the recorded baseline a baseline subject replays.
	BaselineID int64
}

// FrozenDraft is an agent's current draft frozen exactly as Publish would freeze
// it — settings compiled, tools and skills narrowed, sections collected,
// definition digested — but not persisted.
type FrozenDraft struct {
	Definition AgentDefinition
	Settings   EffectiveRelease
}

// Subject identifies the frozen draft by its definition digest.
func (draft FrozenDraft) Subject() AgentEvaluationSubject {
	return AgentEvaluationSubject{Kind: SubjectDraft, AgentID: draft.Definition.AgentID, DefinitionDigest: draft.Definition.DefinitionDigest}
}

// EvaluationRunStatus is the state of a manifest run or a queued agent evaluation.
type EvaluationRunStatus string

const (
	EvaluationQueued       EvaluationRunStatus = "Q"
	EvaluationRunning      EvaluationRunStatus = "R"
	EvaluationCompleted    EvaluationRunStatus = "C"
	EvaluationStoppedEarly EvaluationRunStatus = "S"
	EvaluationFailed       EvaluationRunStatus = "F"
)

// Valid reports whether the status is a known one.
func (status EvaluationRunStatus) Valid() bool {
	switch status {
	case EvaluationQueued, EvaluationRunning, EvaluationCompleted, EvaluationStoppedEarly, EvaluationFailed:
		return true
	}
	return false
}

// Terminal reports whether the status ends a run.
func (status EvaluationRunStatus) Terminal() bool {
	return status == EvaluationCompleted || status == EvaluationStoppedEarly || status == EvaluationFailed
}

// AgentEvaluationRequest asks for one golden set version to run against the
// agent's current draft or one of its releases.
type AgentEvaluationRequest struct {
	AgentID    string
	SetVersion int64
	Draft      bool
	Release    string
}

// AgentEvaluationEnqueue is a validated, pinned evaluation the store queues.
type AgentEvaluationEnqueue struct {
	TenantID   int64
	AgentID    string
	SetVersion int64
	Scope      GoldenScope
	Subject    AgentEvaluationSubject
	// Draft is the frozen definition a draft subject runs; nil for a release.
	Draft *AgentDefinition
	// DraftSettings are the settings frozen with Draft, digested by Draft.SettingsDigest; nil for a release.
	DraftSettings []EffectiveResource
	RequestedBy   PrincipalRef
	// MaxOpenRuns caps the agent's queued and running evaluations; zero refuses every enqueue.
	MaxOpenRuns int
	// Baseline is arm a of a comparison, whose arm b is Subject; nil for a single-subject evaluation.
	Baseline *EvaluationArm
}

// AgentEvaluationRun is one queued agent evaluation with its settled totals.
type AgentEvaluationRun struct {
	RunID       int64
	TenantID    int64
	AgentID     string
	GoldenSetID string
	SetVersion  int64
	Scope       GoldenScope
	Subject     AgentEvaluationSubject
	Status      EvaluationRunStatus
	RequestedBy PrincipalRef
	QueuedAt    time.Time
	StartedAt   *time.Time
	CompletedAt *time.Time
	Samples     int
	Usage       Usage
	// Exponent is the minor-unit exponent of Usage.Currency.
	Exponent int
	// HiddenResults counts results withheld from the reading scope.
	HiddenResults int
	Reason        string
	// Baseline is arm a of a comparison run, whose arm b is Subject; nil for a single-subject run.
	Baseline *AgentEvaluationSubject
}

// AgentEvaluationLease fences one worker's claim on a run; every write names it.
type AgentEvaluationLease struct {
	TenantID int64
	RunID    int64
	Token    int64
	// TTL is how far each recorded result extends the lease on the store clock.
	TTL time.Duration
}

// AgentEvaluationClaim is everything a worker needs to execute a claimed run.
type AgentEvaluationClaim struct {
	Lease AgentEvaluationLease
	Run   AgentEvaluationRun
	// Draft is the pinned frozen definition of a draft subject.
	Draft *AgentDefinition
	// DraftSettings are the settings pinned with Draft, digested by Draft.SettingsDigest.
	DraftSettings []EffectiveResource
	// Recorded are example ids an earlier attempt already settled, with their usage
	// and their evaluators' usage combined.
	Recorded map[string]Usage
	// Unscored are recorded example ids whose scoring failed, with the recorded reason.
	Unscored map[string]string
	// Baseline is arm a of a comparison run with its pinned draft or recorded outputs.
	Baseline *EvaluationArm
}

// AgentCaseResult is one example's scored outcome as the reading scope may see it.
type AgentCaseResult struct {
	ExampleID string
	Scores    []EvaluationScore
	Latency   time.Duration
	Usage     Usage
	// EvaluatorUsage is what model-based evaluators spent scoring the case, in Usage's currency.
	EvaluatorUsage   Usage
	Exponent         int
	NeedsHumanReview bool
	Reason           string
	// Output is the stored answer of a comparison arm; empty when none was stored.
	Output ObjectRef
	// Answer is Output's content, read back and verified for display.
	Answer []byte
	// AnswerOmitted marks a stored answer left out to keep a page within its byte budget.
	AnswerOmitted bool
	// Trajectory is where the case's trajectory was stored; empty when it has none.
	Trajectory ObjectRef
}

// AgentEvaluationPage is one page of a run's results in descending example id
// order; NextBefore continues after the page and is empty on the last one.
type AgentEvaluationPage struct {
	Run        AgentEvaluationRun
	Results    []AgentCaseResult
	NextBefore string
}
