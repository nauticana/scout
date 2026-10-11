package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// EvaluationBaselineStore keeps recorded human baselines of golden set versions.
type EvaluationBaselineStore interface {
	// Record stores one output for every example of the set version visible in scope, and
	// for no other, as a new immutable baseline named baseline.Name.
	Record(ctx context.Context, scope domain.GoldenScope, baseline domain.EvaluationBaseline, outputs []domain.BaselineOutput) (domain.EvaluationBaseline, error)
	// List returns the set version's baselines, newest first.
	List(ctx context.Context, tenantID int64, goldenSetID string, setVersion int64) ([]domain.EvaluationBaseline, error)
}

// StudioCompareBackend compares two evaluation subjects of an agent on a golden set version
// and keeps the human baselines a comparison may name.
type StudioCompareBackend interface {
	Compare(ctx context.Context, actor domain.StudioActor, request domain.AgentCompareRequest) (domain.AgentEvaluationRun, error)
	Comparison(ctx context.Context, actor domain.StudioActor, runID int64, before string, limit int) (domain.AgentComparisonPage, error)
	RecordBaseline(ctx context.Context, actor domain.StudioActor, record domain.BaselineRecord) (domain.EvaluationBaseline, error)
	Baselines(ctx context.Context, actor domain.StudioActor, agentID string, setVersion int64) ([]domain.EvaluationBaseline, error)
}
