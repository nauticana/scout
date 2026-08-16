package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// CaseExecutorFunc adapts a function to contract.CaseExecutor.
type CaseExecutorFunc func(context.Context, domain.EvaluationSubject, domain.GoldenExample, []domain.KnowledgeMatch) (domain.EvaluationCase, error)

// Execute invokes the configured function.
func (function CaseExecutorFunc) Execute(ctx context.Context, subject domain.EvaluationSubject, example domain.GoldenExample, preserved []domain.KnowledgeMatch) (domain.EvaluationCase, error) {
	return function(ctx, subject, example, preserved)
}

// HeuristicEvaluator contains a configurable heuristic version and callback.
type HeuristicEvaluator struct {
	EvaluatorVersion domain.EvaluatorVersion
	EvaluateFunc     func(context.Context, domain.EvaluationCase) ([]domain.EvaluationScore, error)
}

// Version returns the configured evaluator version.
func (evaluator *HeuristicEvaluator) Version() domain.EvaluatorVersion {
	return evaluator.EvaluatorVersion
}

// Evaluate invokes EvaluateFunc when configured.
func (evaluator *HeuristicEvaluator) Evaluate(ctx context.Context, evalCase domain.EvaluationCase) ([]domain.EvaluationScore, error) {
	if evaluator.EvaluateFunc == nil {
		return nil, nil
	}
	return evaluator.EvaluateFunc(ctx, evalCase)
}

// JudgeEvaluator contains a configurable judge version and comparison callback.
type JudgeEvaluator struct {
	EvaluatorVersion domain.EvaluatorVersion
	CompareFunc      func(context.Context, domain.EvaluationPair) (domain.JudgeVerdict, error)
	// Calls counts Compare invocations so cache behavior is observable.
	Calls int
}

// Version returns the configured judge version.
func (judge *JudgeEvaluator) Version() domain.EvaluatorVersion { return judge.EvaluatorVersion }

// Compare invokes CompareFunc and counts the call.
func (judge *JudgeEvaluator) Compare(ctx context.Context, pair domain.EvaluationPair) (domain.JudgeVerdict, error) {
	judge.Calls++
	if judge.CompareFunc == nil {
		return domain.JudgeVerdict{}, nil
	}
	return judge.CompareFunc(ctx, pair)
}

// GateDecisionStore contains configurable gate decision callbacks.
type GateDecisionStore struct {
	PutFunc    func(context.Context, domain.GateDecision) error
	LatestFunc func(context.Context, string) (domain.GateDecision, error)
}

// Put invokes PutFunc when configured.
func (store *GateDecisionStore) Put(ctx context.Context, decision domain.GateDecision) error {
	if store.PutFunc == nil {
		return nil
	}
	return store.PutFunc(ctx, decision)
}

// Latest invokes LatestFunc.
func (store *GateDecisionStore) Latest(ctx context.Context, platformVersion string) (domain.GateDecision, error) {
	return store.LatestFunc(ctx, platformVersion)
}

// OnlineMetricsSource contains a configurable online telemetry callback.
type OnlineMetricsSource struct {
	OnlineFunc func(context.Context, domain.RolloutTarget) (domain.OnlineMetrics, error)
}

// Online invokes OnlineFunc.
func (source *OnlineMetricsSource) Online(ctx context.Context, target domain.RolloutTarget) (domain.OnlineMetrics, error) {
	return source.OnlineFunc(ctx, target)
}

// SamplingPolicyRepository returns one fixed sampling policy.
type SamplingPolicyRepository struct {
	Policy     domain.SamplingPolicy
	PolicyFunc func(context.Context, int64) (domain.SamplingPolicy, error)
}

// SamplingPolicyFor invokes PolicyFunc when configured, otherwise returns Policy.
func (repository *SamplingPolicyRepository) SamplingPolicyFor(ctx context.Context, tenantID int64) (domain.SamplingPolicy, error) {
	if repository.PolicyFunc != nil {
		return repository.PolicyFunc(ctx, tenantID)
	}
	return repository.Policy, nil
}

// EvaluationResultStore contains configurable run and result callbacks.
type EvaluationResultStore struct {
	StartRunFunc    func(context.Context, domain.EvaluationRun) (int64, error)
	FinishRunFunc   func(context.Context, domain.EvaluationRun) error
	PutResultsFunc  func(context.Context, int64, int64, []domain.EvaluationResult) error
	ListResultsFunc func(context.Context, int64, int64) ([]domain.EvaluationResult, error)
}

// StartRun invokes StartRunFunc when configured.
func (store *EvaluationResultStore) StartRun(ctx context.Context, run domain.EvaluationRun) (int64, error) {
	if store.StartRunFunc == nil {
		return 1, nil
	}
	return store.StartRunFunc(ctx, run)
}

// FinishRun invokes FinishRunFunc when configured.
func (store *EvaluationResultStore) FinishRun(ctx context.Context, run domain.EvaluationRun) error {
	if store.FinishRunFunc == nil {
		return nil
	}
	return store.FinishRunFunc(ctx, run)
}

// PutResults invokes PutResultsFunc when configured.
func (store *EvaluationResultStore) PutResults(ctx context.Context, tenantID int64, runID int64, results []domain.EvaluationResult) error {
	if store.PutResultsFunc == nil {
		return nil
	}
	return store.PutResultsFunc(ctx, tenantID, runID, results)
}

// ListResults invokes ListResultsFunc when configured.
func (store *EvaluationResultStore) ListResults(ctx context.Context, tenantID int64, runID int64) ([]domain.EvaluationResult, error) {
	if store.ListResultsFunc == nil {
		return nil, nil
	}
	return store.ListResultsFunc(ctx, tenantID, runID)
}

var _ contract.CaseExecutor = CaseExecutorFunc(nil)
var _ contract.HeuristicEvaluator = (*HeuristicEvaluator)(nil)
var _ contract.JudgeEvaluator = (*JudgeEvaluator)(nil)
var _ contract.GateDecisionStore = (*GateDecisionStore)(nil)
var _ contract.OnlineMetricsSource = (*OnlineMetricsSource)(nil)
var _ contract.SamplingPolicyRepository = (*SamplingPolicyRepository)(nil)
var _ contract.EvaluationResultStore = (*EvaluationResultStore)(nil)
