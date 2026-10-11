package domain

import "time"

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
	SubjectDraft   EvaluationSubjectKind = "draft"
	SubjectRelease EvaluationSubjectKind = "release"
)

// AgentEvaluationSubject pins one arm of an agent evaluation: a frozen draft
// identified by its definition digest, or a published release.
type AgentEvaluationSubject struct {
	Kind    EvaluationSubjectKind
	AgentID string
	// Version is the release a release subject runs; empty for a draft.
	Version          string
	DefinitionDigest string
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

// Evaluation run statuses shared by manifest runs and queued agent runs.
const (
	EvaluationQueued       = "queued"
	EvaluationRunning      = "running"
	EvaluationCompleted    = "completed"
	EvaluationStoppedEarly = "stopped_early"
	EvaluationFailed       = "failed"
)

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
	Draft       *AgentDefinition
	RequestedBy PrincipalRef
	// MaxOpenRuns caps the agent's queued and running evaluations; zero refuses every enqueue.
	MaxOpenRuns int
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
	Status      string
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
	// Recorded are example ids an earlier attempt already settled, with their usage.
	Recorded map[string]Usage
}

// AgentCaseResult is one example's scored outcome as the reading scope may see it.
type AgentCaseResult struct {
	ExampleID        string
	Scores           []EvaluationScore
	Latency          time.Duration
	Usage            Usage
	Exponent         int
	NeedsHumanReview bool
	Reason           string
}

// AgentEvaluationPage is one page of a run's results in descending example id
// order; NextBefore continues after the page and is empty on the last one.
type AgentEvaluationPage struct {
	Run        AgentEvaluationRun
	Results    []AgentCaseResult
	NextBefore string
}
