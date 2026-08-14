package dataplane

import (
	"context"
	"log"
	"time"

	"github.com/nauticana/keel/port"

	"github.com/nauticana/scout/domain"
)

// BaseRecorder emits the agent-runtime metric vocabulary over keel's metrics
// boundary. A nil receiver or backend is a no-op, so call sites need no
// metrics-enabled branch. Embed it to add product-specific metrics.
type BaseRecorder struct {
	port.MetricsRecorder
}

func (r *BaseRecorder) enabled() bool { return r != nil && r.MetricsRecorder != nil }

// RecordTurn records one finished agent task execution: latency, outcome,
// provider, model, release version, token, and cost dimensions.
func (r *BaseRecorder) RecordTurn(ctx context.Context, taskKind, outcome string, ref domain.ModelReference, agentVersion string, latency time.Duration, usage domain.Usage) {
	if !r.enabled() {
		return
	}
	if latency < 0 {
		latency = 0
	}
	labels := map[string]string{
		"task_kind": taskKind, "outcome": outcome,
		"provider": ref.ProviderID, "model": ref.ModelID, "agent_version": agentVersion,
	}
	for _, m := range []port.MetricMeasurement{
		{Name: "agent_turn_duration_seconds", Help: "Agent task execution latency to its terminal state.", Kind: port.MetricHistogram, Value: latency.Seconds(), Labels: labels},
		{Name: "agent_turn_tokens_total", Help: "Model tokens consumed by finished agent tasks.", Kind: port.MetricCounter, Value: float64(usage.InputTokens + usage.OutputTokens), Labels: labels},
		{Name: "agent_turn_cost_minor_units_total", Help: "Settled agent task cost in catalog minor units.", Kind: port.MetricCounter, Value: float64(usage.CostMinorUnits), Labels: labels},
	} {
		if err := r.RecordMetric(ctx, m); err != nil {
			log.Printf("observability %s: %v", m.Name, err)
		}
	}
}

// RecordCapability counts capability evaluations per task and readiness
// outcome, so dashboards can see what a task surface is actually offering.
func (r *BaseRecorder) RecordCapability(ctx context.Context, taskKind string, status domain.AgentReadiness) {
	if !r.enabled() {
		return
	}
	err := r.RecordMetric(ctx, port.MetricMeasurement{
		Name: "agent_capability_status_total", Help: "Capability evaluations by task and readiness outcome.",
		Kind: port.MetricCounter, Value: 1,
		Labels: map[string]string{"task_kind": taskKind, "status": string(status)},
	})
	if err != nil {
		log.Printf("observability agent_capability_status_total: %v", err)
	}
}
