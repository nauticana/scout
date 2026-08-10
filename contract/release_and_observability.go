package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// AgentContractTestRunner validates agents against a proposed platform build.
type AgentContractTestRunner interface {
	// Run evaluates a proposed platform build against tenant agent contracts.
	Run(ctx context.Context, platformVersion string, cases []domain.ContractTestCase) ([]domain.ContractTestResult, error)
}

// TenantCorpusSampler selects representative tenant agents for compatibility tests.
type TenantCorpusSampler interface {
	// Sample returns a risk-stratified corpus across tenant agents and capabilities.
	Sample(ctx context.Context, platformVersion string, limit int) ([]domain.ContractTestCase, error)
}

// PlatformReleaseRolloutController manages platform releases by tenant ring.
type PlatformReleaseRolloutController interface {
	// Start begins a staged platform rollout at the first tenant ring.
	Start(ctx context.Context, target domain.RolloutTarget) error
	// Advance moves a healthy platform build to the next tenant ring.
	Advance(ctx context.Context, platformVersion string) error
	// Halt stops assigning new traffic to a platform build.
	Halt(ctx context.Context, platformVersion, reason string) error
	// Rollback restores affected tenant rings to the prior platform build.
	Rollback(ctx context.Context, platformVersion string) error
}

// RolloutHealthEvaluator determines whether a staged release may advance.
type RolloutHealthEvaluator interface {
	// Healthy evaluates latency, errors, cost, quality, and contract regressions.
	Healthy(ctx context.Context, target domain.RolloutTarget) (bool, error)
}

// AuditSink records durable tenant-scoped security and governance events.
type AuditSink interface {
	// Record durably writes a redacted tenant-scoped audit event.
	Record(ctx context.Context, event domain.AuditEvent) error
}

// RuntimeMetrics records data-plane latency, usage, and failure signals.
type RuntimeMetrics interface {
	// RecordTurn records latency, outcome, version, and usage dimensions.
	RecordTurn(ctx context.Context, request domain.TurnRequest, result domain.TurnResult, err error)
	// RecordStep records latency, outcome, and usage for one graph step.
	RecordStep(ctx context.Context, tenantID int64, agentID string, step domain.ExecutionStep, result domain.StepResult, err error)
	// RecordDependency records a governed model or tool dependency outcome.
	RecordDependency(ctx context.Context, tenantID int64, dependency, operation string, usage domain.Usage, err error)
}
