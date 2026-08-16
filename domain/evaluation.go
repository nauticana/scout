package domain

import "time"

// EvaluationRole names one arm of a paired baseline/candidate evaluation.
type EvaluationRole string

const (
	RoleBaseline  EvaluationRole = "baseline"
	RoleCandidate EvaluationRole = "candidate"
)

// GoldenScope is the authorization scope a golden set is read under; the gate
// scope is the only one that can see hidden examples.
type GoldenScope string

const (
	GoldenScopeDev  GoldenScope = "dev"
	GoldenScopeGate GoldenScope = "gate"
)

// Metric names shared by evaluators, paired scoring, gate decisions, and retrieval evaluation.
const (
	MetricCorrectness          = "correctness"
	MetricRetrievalRecall      = "retrieval_recall"
	MetricCitationSupport      = "citation_support"
	MetricGroundedness         = "groundedness"
	MetricSafety               = "safety"
	MetricInstructionFollowing = "instruction_following"
	MetricSchemaValid          = "schema_valid"
	MetricPatternMatch         = "pattern_match"
	MetricLatencyMs            = "latency_ms"
	MetricTokens               = "tokens"
	MetricCostMinorUnits       = "cost_minor_units"

	MetricToolChoice           = "tool_choice"
	MetricToolArguments        = "tool_arguments"
	MetricPolicyCompliance     = "policy_compliance"
	MetricTrajectoryEfficiency = "trajectory_efficiency"
	MetricRecoverability       = "recoverability"
	MetricFinalState           = "final_state"
	MetricTimeToFirstMs        = "time_to_first_ms"
	MetricInterruptibility     = "interruptibility"
	MetricPartialSafety        = "partial_safety"

	MetricRecallAtK          = "recall_at_k"
	MetricMRR                = "mrr"
	MetricNDCG               = "ndcg"
	MetricFilterSelectivity  = "filter_selectivity"
	MetricCitationPrecision  = "citation_precision"
	MetricAbstentionQuality  = "abstention_quality"
	MetricIngestionLagMs     = "ingestion_lag_ms"
	MetricTombstoneLagMs     = "tombstone_lag_ms"
	MetricAuthorizationLeaks = "authorization_leaks"
)

// SliceAll is the aggregate slice every paired delta is reported under.
const SliceAll = "all"

// EvaluationSubject pins one arm of an evaluation to reproducible versions.
type EvaluationSubject struct {
	AgentID         string
	AgentVersion    string
	Versions        ComponentVersions
	IndexGeneration int64
	// Decoding is the canonical JSON of sampling settings (temperature, top-p, seed, max tokens).
	Decoding []byte
}

// EvaluatorVersion identifies one heuristic, judge, or human evaluator revision.
type EvaluatorVersion struct {
	Kind    string
	Version string
}

// EvaluationManifest is the immutable, content-addressed description of one paired evaluation.
type EvaluationManifest struct {
	// ManifestID is the SHA-256 hex digest of the canonical JSON of every other field except CreatedAt.
	ManifestID          string
	TenantID            int64
	Candidate           EvaluationSubject
	Baseline            EvaluationSubject
	GoldenSetID         string
	GoldenSetVersion    int64
	DatasetRevision     string
	Evaluators          []EvaluatorVersion
	SafetyPolicyVersion string
	CreatedAt           time.Time
}

// ExampleReview is one entry in a golden example's review history.
type ExampleReview struct {
	Reviewer   string
	Verdict    string
	Notes      string
	ReviewedAt time.Time
}

// GoldenExample is the metadata of one evaluation example; the payload lives in object storage.
type GoldenExample struct {
	TenantID         int64
	ExampleID        string
	GoldenSetID      string
	SetVersion       int64
	Provenance       string
	ConsentClass     string
	RetentionClass   string
	RiskTier         string
	Domain           string
	Language         string
	RubricRef        string
	ExpectedBehavior []byte
	Payload          ObjectRef
	// Hidden marks a gate example that dev-scope reads never return.
	Hidden  bool
	Reviews []ExampleReview
}

// GoldenQuery is one retrieval example with the principal it must be answered for.
type GoldenQuery struct {
	TenantID            int64
	QueryID             string
	GoldenSetID         string
	SetVersion          int64
	KnowledgeBaseID     string
	Query               []byte
	Principal           string
	Entitlements        []byte
	ExpectedDocumentIDs []string
	ExpectAbstention    bool
}

// GoldenSetVersion is one frozen revision of a golden set.
type GoldenSetVersion struct {
	TenantID        int64
	GoldenSetID     string
	SetVersion      int64
	DatasetRevision string
	ExampleCount    int
	FrozenAt        time.Time
}

// TrajectoryEventKind classifies one event of an agentic trace.
type TrajectoryEventKind string

const (
	TrajectoryToken       TrajectoryEventKind = "token"
	TrajectoryToolCall    TrajectoryEventKind = "tool_call"
	TrajectoryObservation TrajectoryEventKind = "observation"
	TrajectoryState       TrajectoryEventKind = "state"
	TrajectoryPolicy      TrajectoryEventKind = "policy"
)

// TrajectoryEvent is one sequenced element of an agentic or streaming trace.
type TrajectoryEvent struct {
	Sequence int64
	Kind     TrajectoryEventKind
	// Name is the tool id, state name, or policy id the event refers to.
	Name    string
	Payload []byte
	// Offset is the time since the turn started.
	Offset time.Duration
	Usage  Usage
	// Recovered marks an event that repaired an earlier failed step.
	Recovered bool
	Failed    bool
}

// EvaluationCase is one arm's replayed output for one example.
type EvaluationCase struct {
	ManifestID string
	Example    GoldenExample
	Role       EvaluationRole
	Output     []byte
	Retrieval  []KnowledgeMatch
	Trajectory []TrajectoryEvent
	Latency    time.Duration
	Usage      Usage
}

// EvaluationPair is the blinded input to a pairwise judge.
type EvaluationPair struct {
	Example   GoldenExample
	Baseline  EvaluationCase
	Candidate EvaluationCase
}

// EvaluationScore is one metric value from one evaluator.
type EvaluationScore struct {
	Metric     string
	Value      float64
	Confidence float64
	Evaluator  EvaluatorVersion
	Rationale  string
	// Critical marks a safety or authorization failure that blocks promotion by itself.
	Critical bool
}

// JudgeVerdict is a pairwise judge's scores mapped back to roles.
type JudgeVerdict struct {
	Baseline   []EvaluationScore
	Candidate  []EvaluationScore
	Preferred  EvaluationRole
	Confidence float64
	Rationale  string
	// InputDigest identifies the blinded judge input for caching and audit.
	InputDigest string
}

// EvaluationResult is every score one arm earned on one example.
type EvaluationResult struct {
	ManifestID       string
	ExampleID        string
	Role             EvaluationRole
	Scores           []EvaluationScore
	Latency          time.Duration
	Usage            Usage
	NeedsHumanReview bool
	Reason           string
}

// EvaluationRun is one execution of a manifest.
type EvaluationRun struct {
	RunID       int64
	TenantID    int64
	ManifestID  string
	Scope       GoldenScope
	Status      string
	StartedAt   time.Time
	CompletedAt time.Time
	Samples     int
	Usage       Usage
}

// SliceDelta is the paired candidate-minus-baseline effect for one metric on one slice.
type SliceDelta struct {
	Slice     string
	Metric    string
	Baseline  float64
	Candidate float64
	Delta     float64
	CILow     float64
	CIHigh    float64
	Samples   int
}

// EvaluationSummary aggregates one run into the evidence a gate decides on.
type EvaluationSummary struct {
	ManifestID         string
	Deltas             []SliceDelta
	Samples            int
	CriticalFailures   int
	HumanReviewPending int
	Usage              Usage
	Promotable         bool
	Reasons            []string
	StoppedEarly       bool
}

// GateApproval is one explicit human exemption attached to a gate decision.
type GateApproval struct {
	Approver   string
	Scope      string
	Reason     string
	ApprovedAt time.Time
}

// GateDecision is the signed, expiring outcome a rollout controller consumes.
type GateDecision struct {
	DecisionID       string
	TenantID         int64
	ManifestID       string
	PlatformVersion  string
	DatasetRevision  string
	JudgeVersions    []EvaluatorVersion
	Deltas           []SliceDelta
	Confidence       float64
	Verdict          RolloutVerdict
	Exemptions       []GateApproval
	IssuedAt         time.Time
	ExpiresAt        time.Time
	TelemetryFreshAt time.Time
	Signature        []byte
	SignerKeyID      string
}

// OnlineMetrics is the fresh production telemetry a gate is cross-checked against.
type OnlineMetrics struct {
	Samples    int64
	Window     time.Duration
	ObservedAt time.Time
	// Breached lists metrics outside their online thresholds; empty means within bounds.
	Breached []string
}

// HumanReviewItem is one paired example routed to a human reviewer.
type HumanReviewItem struct {
	ItemID     string
	TenantID   int64
	ManifestID string
	ExampleID  string
	RiskTier   string
	Reason     string
	Pair       EvaluationPair
	Verdict    JudgeVerdict
	EnqueuedAt time.Time
	Review     ExampleReview
	Resolved   bool
}

// SamplingPolicy bounds which production turns may become evaluation samples for one tenant.
type SamplingPolicy struct {
	TenantID int64
	// BaseRate, RiskRate, and UncertaintyRate are probabilities in [0,1] for
	// ordinary, high-risk, and low-confidence turns respectively.
	BaseRate        float64
	RiskRate        float64
	UncertaintyRate float64
	// MaxPerWindow caps samples per tenant per Window regardless of rate.
	MaxPerWindow int64
	Window       time.Duration
	OptOut       bool
	// RedactionRequired rejects any sample not marked redacted before storage.
	RedactionRequired bool
	ResidencyRegion   string
	RetentionClass    string
	Retention         time.Duration
}

// SampleSignal is the content-free description of one production turn offered to the sampler.
type SampleSignal struct {
	TenantContext TenantContext
	RequestID     string
	AgentID       string
	AgentVersion  string
	RiskScore     float64
	Uncertainty   float64
	Redacted      bool
	// Feedback carries joined outcome signals: explicit rating, retry, escalation, completion.
	Feedback map[string]float64
}

// EvaluationSample is one stored production sample; the payload is encrypted in object storage.
type EvaluationSample struct {
	SampleID       string
	TenantID       int64
	RequestID      string
	AgentID        string
	AgentVersion   string
	Reason         string
	RiskScore      float64
	Uncertainty    float64
	Redacted       bool
	Payload        ObjectRef
	RetentionClass string
	Region         string
	SampledAt      time.Time
	ExpiresAt      time.Time
}

// ProductionFailure is one deduplicated failure awaiting adjudication into a golden set.
type ProductionFailure struct {
	FailureID   string
	TenantID    int64
	SampleID    string
	Digest      string
	Occurrences int
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	Adjudicated bool
	Accepted    bool
	Reviewer    string
}

// CalibrationLabel pairs one human label with the judge score for the same example and metric.
type CalibrationLabel struct {
	ExampleID     string
	Metric        string
	HumanValue    float64
	JudgeValue    float64
	HumanCritical bool
	JudgeCritical bool
}

// PositionTrial records one pairwise judgment shown in both presentation orders.
type PositionTrial struct {
	ExampleID string
	// PreferredAFirst and PreferredASwapped report whether output A won when shown first and when shown second.
	PreferredAFirst   bool
	PreferredASwapped bool
	// AIsJudgeFamily marks output A as produced by the judge's own model family.
	AIsJudgeFamily bool
}

// CalibrationReport is judge–human agreement for one judge version.
type CalibrationReport struct {
	JudgeVersion       EvaluatorVersion
	Kappa              float64
	Alpha              float64
	Precision          float64
	Recall             float64
	PositionBias       float64
	SelfPreferenceBias float64
	Samples            int
}

// DriftReport is the outcome of comparing a recent score window against a reference.
type DriftReport struct {
	Metric        string
	ReferenceMean float64
	CurrentMean   float64
	EffectSize    float64
	Samples       int
	Sustained     bool
}

// RetrievalMetrics aggregates retrieval quality over one golden query set.
type RetrievalMetrics struct {
	K                  int
	Samples            int
	RecallAtK          float64
	MRR                float64
	NDCG               float64
	FilterSelectivity  float64
	CitationPrecision  float64
	AbstentionQuality  float64
	IngestionLag       time.Duration
	TombstoneLag       time.Duration
	AuthorizationLeaks int
}

// RetrievalObservation is one golden query's replayed retrieval and the freshness facts around it.
type RetrievalObservation struct {
	Query GoldenQuery
	// Matches are the results returned; Authorized reports per match whether the golden principal may see it.
	Matches    []KnowledgeMatch
	Authorized []bool
	// CandidateCount is the pre-filter candidate population when the index reports it.
	CandidateCount int
	// CitedDocumentIDs are the documents the generated answer cited, when generation ran.
	CitedDocumentIDs []string
	Abstained        bool
	IngestionLag     time.Duration
	TombstoneLag     time.Duration
}

// AblationArm is one factorial arm that changes a single component of the baseline.
type AblationArm struct {
	Component string
	Subject   EvaluationSubject
}

// PromotionPolicy states what paired evidence must show before a candidate may promote.
type PromotionPolicy struct {
	MinSamples int
	// PrimaryMetric drives early stopping and the headline verdict.
	PrimaryMetric string
	// Tolerances is the maximum regression per metric; the CI lower bound of the delta must not fall below its negation.
	Tolerances map[string]float64
	// ProtectedSlices must also satisfy Tolerances, e.g. "language:tr-TR"; every "risk:<tier>" slice for HighRiskTiers is protected implicitly.
	ProtectedSlices     []string
	HighRiskTiers       []string
	MaxCriticalFailures int
	// ConfidenceLevel is the bootstrap interval level in (0,1); zero means 0.95.
	ConfidenceLevel float64
}
