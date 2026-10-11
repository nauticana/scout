package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// DraftFreezer freezes an agent's current draft exactly as Publish would, without
// persisting it, after authorizing the actor to TEST the agent. A non-nil textModel
// replaces the draft's text model before freezing, validated as a draft's own would be.
// The definition digest identifies the frozen draft as an evaluation subject.
type DraftFreezer interface {
	FreezeDraft(ctx context.Context, actor domain.StudioActor, agentID string, textModel *domain.ModelReference) (domain.FrozenDraft, error)
}

// GoldenCaseStore keeps the editable working copy of golden sets and which set
// each agent uses. Gate cases are invisible and immutable in the dev scope;
// Freeze snapshots every case of both scopes into a new immutable set version.
type GoldenCaseStore interface {
	// SetOwner returns the agent owning the golden set, empty when no agent owns it, or domain.ErrNotFound.
	SetOwner(ctx context.Context, tenantID int64, goldenSetID string) (string, error)
	// Associate points the agent at the set; an existing set owned by another than association.Owner is domain.ErrConflict.
	Associate(ctx context.Context, association domain.GoldenSetAssociation) error
	// AgentSet returns the agent's golden set id and working revision, or domain.ErrNotFound.
	AgentSet(ctx context.Context, tenantID int64, agentID string) (string, int64, error)
	// Cases returns the agent's set as the scope sees it; an agent with no set yields an empty GoldenSetID.
	Cases(ctx context.Context, scope domain.GoldenScope, tenantID int64, agentID string) (domain.GoldenCaseSet, error)
	PutCase(ctx context.Context, scope domain.GoldenScope, goldenCase domain.GoldenCase, expectedRevision int64) (int64, error)
	RemoveCase(ctx context.Context, scope domain.GoldenScope, tenantID int64, goldenSetID, caseID string, expectedRevision int64) (int64, error)
	Freeze(ctx context.Context, tenantID int64, goldenSetID string, expectedRevision int64) (domain.GoldenSetVersion, int64, error)
}

// AgentEvaluationStore queues agent evaluations and reads them back for one scope.
type AgentEvaluationStore interface {
	Enqueue(ctx context.Context, request domain.AgentEvaluationEnqueue) (domain.AgentEvaluationRun, error)
	// Run returns an agent run of the tenant; a manifest run is not found.
	Run(ctx context.Context, scope domain.GoldenScope, tenantID, runID int64) (domain.AgentEvaluationRun, error)
	// Results pages a run's results visible in the scope in descending example id, strictly before before when set.
	Results(ctx context.Context, scope domain.GoldenScope, tenantID, runID int64, before string, limit int) ([]domain.AgentCaseResult, error)
	// Comparisons pages a comparison run's examples visible in the scope like Results.
	Comparisons(ctx context.Context, scope domain.GoldenScope, tenantID, runID int64, before string, limit int) ([]domain.AgentCaseComparison, error)
	// Tally counts a comparison run's examples visible in the scope by verdict.
	Tally(ctx context.Context, scope domain.GoldenScope, tenantID, runID int64) (domain.ComparisonTally, error)
}

// AgentEvaluationQueue is the worker side of queued agent evaluations. Every
// write is fenced by the claim's lease token; a lost lease is domain.ErrConflict.
type AgentEvaluationQueue interface {
	Load(ctx context.Context, lease domain.AgentEvaluationLease) (domain.AgentEvaluationClaim, error)
	// Record stores one result and renews the lease; replaying a recorded example is a no-op.
	// A result the run's totals cannot hold, such as another currency, is domain.ErrValidation.
	Record(ctx context.Context, lease domain.AgentEvaluationLease, result domain.EvaluationResult) error
	// RecordComparison stores one example's two arms and verdict like Record.
	RecordComparison(ctx context.Context, lease domain.AgentEvaluationLease, comparison domain.CaseComparison) error
	// Finish settles the run's totals from its recorded results and releases the lease.
	Finish(ctx context.Context, lease domain.AgentEvaluationLease, status, reason string) error
}

// StudioEvaluationBackend serves an agent's saved test cases and evaluations to Studio.
type StudioEvaluationBackend interface {
	Cases(ctx context.Context, actor domain.StudioActor, agentID string) (domain.GoldenCaseSet, error)
	ChangeCases(ctx context.Context, actor domain.StudioActor, change domain.GoldenCaseChange) (domain.GoldenCaseSet, error)
	FreezeCases(ctx context.Context, actor domain.StudioActor, agentID string, expectedRevision int64) (domain.GoldenCaseSet, domain.GoldenSetVersion, error)
	Evaluate(ctx context.Context, actor domain.StudioActor, request domain.AgentEvaluationRequest) (domain.AgentEvaluationRun, error)
	Evaluation(ctx context.Context, actor domain.StudioActor, runID int64, before string, limit int) (domain.AgentEvaluationPage, error)
}
