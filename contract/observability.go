package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

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

// ObservationRecorder is the optional structured capability of a metrics adapter;
// stage wrappers emit through it when the injected RuntimeMetrics implements it.
type ObservationRecorder interface {
	RecordObservation(ctx context.Context, observation domain.Observation)
}

// MetricLabelSink receives already-bounded label sets; adapters never see raw
// tenant identity here and must not retain or mutate the label map.
type MetricLabelSink interface {
	Observe(ctx context.Context, name string, labels map[string]string, value float64)
}

// TenantLedger is the exact per-tenant accounting path; it is the only
// observability consumer allowed to key on tenant identity.
type TenantLedger interface {
	RecordTenantObservation(ctx context.Context, observation domain.Observation) error
}
