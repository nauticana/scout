package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// HeuristicEvaluator scores one arm deterministically without a model call.
type HeuristicEvaluator interface {
	Version() domain.EvaluatorVersion
	Evaluate(ctx context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error)
}

// JudgeEvaluator compares both arms of one example. Implementations blind the
// roles, randomize presentation order, and show only rubric and evidence.
type JudgeEvaluator interface {
	Version() domain.EvaluatorVersion
	Compare(ctx context.Context, pair domain.EvaluationPair) (domain.JudgeVerdict, error)
}

// HumanReviewQueue holds production turns awaiting a human grade. Items are tenant-scoped
// and hold references only, never content. Doubtful evaluation pairs are not queued here:
// the runner marks them in their EvaluationResult.
type HumanReviewQueue interface {
	// Enqueue adds a turn once; a replay is a no-op. An item names only its turn's
	// RequestID: the store derives the conversation, turn, agent, and release, and refuses
	// a turn that is not completed. An agent at the pending cap is domain.ErrRateLimited.
	Enqueue(ctx context.Context, item domain.HumanReviewItem) error
	Get(ctx context.Context, tenantID, itemID int64) (domain.HumanReviewItem, error)
	List(ctx context.Context, tenantID int64, filter domain.ReviewFilter) ([]domain.HumanReviewItem, error)
	// Grade moves a pending item to graded once; replaying the same grade by the same
	// reviewer returns the item, any other grade of a resolved item is domain.ErrConflict.
	Grade(ctx context.Context, tenantID, itemID int64, grade domain.ReviewGrade, reviewer domain.PrincipalRef) (domain.HumanReviewItem, error)
	// Dismiss moves a pending item to dismissed once, with the same replay rule as Grade.
	Dismiss(ctx context.Context, tenantID, itemID int64, notes string, reviewer domain.PrincipalRef) (domain.HumanReviewItem, error)
	// Promoted marks a graded item promoted into the named golden case; replaying the
	// same case returns the item.
	Promoted(ctx context.Context, tenantID, itemID int64, goldenSetID, caseID string) (domain.HumanReviewItem, error)
}

// EvaluationManifestStore persists immutable manifests by content id.
type EvaluationManifestStore interface {
	// Put stores a manifest; an identical replay is a no-op and a different body under the same id is a conflict.
	Put(ctx context.Context, manifest domain.EvaluationManifest) error
	Get(ctx context.Context, tenantID int64, manifestID string) (domain.EvaluationManifest, error)
}

// GoldenSetStore reads and writes golden examples under an authorization scope;
// hidden examples are only visible in the gate scope.
type GoldenSetStore interface {
	PutExample(ctx context.Context, scope domain.GoldenScope, example domain.GoldenExample) error
	GetExample(ctx context.Context, scope domain.GoldenScope, tenantID int64, goldenSetID string, setVersion int64, exampleID string) (domain.GoldenExample, error)
	ListExamples(ctx context.Context, scope domain.GoldenScope, tenantID int64, goldenSetID string, setVersion int64) ([]domain.GoldenExample, error)
	PutQuery(ctx context.Context, scope domain.GoldenScope, query domain.GoldenQuery) error
	ListQueries(ctx context.Context, scope domain.GoldenScope, tenantID int64, goldenSetID string, setVersion int64) ([]domain.GoldenQuery, error)
	FreezeVersion(ctx context.Context, version domain.GoldenSetVersion) error
}

// EvaluationResultStore persists runs and their per-arm results.
type EvaluationResultStore interface {
	StartRun(ctx context.Context, run domain.EvaluationRun) (int64, error)
	FinishRun(ctx context.Context, run domain.EvaluationRun) error
	PutResults(ctx context.Context, tenantID int64, runID int64, results []domain.EvaluationResult) error
	ListResults(ctx context.Context, tenantID int64, runID int64) ([]domain.EvaluationResult, error)
}

// GateDecisionStore persists signed gate decisions and serves the latest one per platform build.
type GateDecisionStore interface {
	Put(ctx context.Context, decision domain.GateDecision) error
	Latest(ctx context.Context, platformVersion string) (domain.GateDecision, error)
}

// GateSigner signs and verifies the canonical bytes of a gate decision.
type GateSigner interface {
	Sign(ctx context.Context, payload []byte) (signature []byte, keyID string, err error)
	Verify(ctx context.Context, payload, signature []byte, keyID string) error
}

// CaseExecutor replays one example against one subject; preservedRetrieval,
// when non-nil, must be used instead of retrieving again.
type CaseExecutor interface {
	Execute(ctx context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, preservedRetrieval []domain.KnowledgeMatch) (domain.EvaluationCase, error)
}

// CaseRetriever supplies the knowledge a replayed example is answered from: it
// retrieves, or reuses preserved when non-nil, and returns the turn input
// carrying the matches.
type CaseRetriever interface {
	Retrieve(ctx context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, input []byte, preserved []domain.KnowledgeMatch) (grounded []byte, matches []domain.KnowledgeMatch, err error)
}

// ObjectReader reads the content an object reference names; the caller verifies its digest.
type ObjectReader interface {
	Read(ctx context.Context, ref domain.ObjectRef) ([]byte, error)
}

// EvaluationRunner replays baseline and candidate over identical examples and returns paired evidence.
type EvaluationRunner interface {
	Run(ctx context.Context, manifest domain.EvaluationManifest, examples []domain.GoldenExample) (domain.EvaluationSummary, error)
}

// ProductionSampler decides whether one production turn becomes an evaluation sample.
type ProductionSampler interface {
	// Sample returns the sampling reason when the turn is selected; an empty reason means skipped.
	Sample(ctx context.Context, signal domain.SampleSignal) (reason string, err error)
}

// SamplingPolicyRepository supplies each tenant's sampling policy.
type SamplingPolicyRepository interface {
	SamplingPolicyFor(ctx context.Context, tenantID int64) (domain.SamplingPolicy, error)
}

// SampleStore keeps encrypted production samples and returns only what the tenant may read.
type SampleStore interface {
	Put(ctx context.Context, sample domain.EvaluationSample, payload []byte) (domain.EvaluationSample, error)
	Get(ctx context.Context, tenantID int64, sampleID string) (domain.EvaluationSample, []byte, error)
	Delete(ctx context.Context, tenantID int64, sampleID string) error
}

// FailureAdjudicator deduplicates production failures and records the human decision before promotion to a golden set.
type FailureAdjudicator interface {
	// Report folds an observed failure into its digest bucket and returns the bucket.
	Report(ctx context.Context, tenantID int64, sampleID string, failure []byte) (domain.ProductionFailure, error)
	// Adjudicate records the reviewer's accept/reject verdict; accepted failures may become golden examples.
	Adjudicate(ctx context.Context, tenantID int64, failureID, reviewer string, accepted bool) (domain.ProductionFailure, error)
	Pending(ctx context.Context, tenantID int64, limit int) ([]domain.ProductionFailure, error)
}

// DriftDetector compares recent score distributions against a reference window.
type DriftDetector interface {
	Detect(ctx context.Context, metric string, reference, current []float64) (domain.DriftReport, error)
}

// JudgeCalibrator measures judge–human agreement for one judge version.
type JudgeCalibrator interface {
	Calibrate(ctx context.Context, judge domain.EvaluatorVersion, labels []domain.CalibrationLabel, trials []domain.PositionTrial) (domain.CalibrationReport, error)
}

// TrajectoryEvaluator scores an agentic or streaming trace.
type TrajectoryEvaluator interface {
	Version() domain.EvaluatorVersion
	Score(ctx context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error)
}

// ToolSandbox answers tool calls deterministically during replay and records what was asked.
type ToolSandbox interface {
	Invoke(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error)
	Recorded() []domain.ToolCall
	Reset()
}

// RetrievalEvaluator scores replayed golden queries independently of generation.
type RetrievalEvaluator interface {
	Evaluate(ctx context.Context, manifestID string, observations []domain.RetrievalObservation) (domain.RetrievalMetrics, []domain.EvaluationResult, error)
}

// OnlineMetricsSource supplies fresh production telemetry for a rollout target.
type OnlineMetricsSource interface {
	Online(ctx context.Context, target domain.RolloutTarget) (domain.OnlineMetrics, error)
}
