package observability

import (
	"context"
	"fmt"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/stage"
)

// StageTurn is the stage label of a whole-turn observation (empty Stage).
const StageTurn = "turn"

// ComponentTurn names whole-turn observations built from RecordTurn.
const ComponentTurn = "turn"

// BoundedRuntimeMetricsConfig configures BoundedRuntimeMetrics.
type BoundedRuntimeMetricsConfig struct {
	// Sink receives every fleet series; required.
	Sink contract.MetricLabelSink
	// Policy bounds label keys and values; the zero value is DefaultLabelPolicy.
	Policy LabelPolicy
	// Ledger is the optional exact per-tenant path; it alone sees tenant identity.
	Ledger contract.TenantLedger
	// Release stamps every series with the running platform release.
	Release string
}

// BoundedRuntimeMetrics turns RuntimeMetrics calls and structured observations
// into catalog series carrying only allowlisted labels; raw tenant identity
// only ever reaches the optional TenantLedger.
type BoundedRuntimeMetrics struct {
	config BoundedRuntimeMetricsConfig
	policy compiledPolicy
}

var _ contract.RuntimeMetrics = (*BoundedRuntimeMetrics)(nil)
var _ contract.ObservationRecorder = (*BoundedRuntimeMetrics)(nil)

// NewBoundedRuntimeMetrics validates the config and builds the recorder.
func NewBoundedRuntimeMetrics(config BoundedRuntimeMetricsConfig) (*BoundedRuntimeMetrics, error) {
	if config.Sink == nil {
		return nil, fmt.Errorf("bounded runtime metrics: sink is required")
	}
	if err := config.Policy.Validate(); err != nil {
		return nil, fmt.Errorf("bounded runtime metrics: %w", err)
	}
	config.Release = strings.TrimSpace(config.Release)
	if _, err := config.Policy.Sanitize(map[string]string{LabelRelease: config.Release}); err != nil {
		return nil, fmt.Errorf("bounded runtime metrics: release: %w", err)
	}
	return &BoundedRuntimeMetrics{config: config, policy: config.Policy.compile()}, nil
}

// RecordTurn records the turn as a whole-turn observation.
func (metrics *BoundedRuntimeMetrics) RecordTurn(ctx context.Context, request domain.TurnRequest, result domain.TurnResult, err error) {
	observation := domain.Observation{
		TenantID:      request.TenantContext.TenantID,
		TenantTier:    request.TenantContext.Tier,
		PriorityClass: request.TenantContext.PriorityClass,
		Region:        request.TenantContext.Region,
		Component:     ComponentTurn,
		Versions:      domain.ComponentVersions{Agent: result.AgentVersion, Release: metrics.config.Release},
		Usage:         result.Usage,
	}
	metrics.finish(&observation, err)
	metrics.record(ctx, observation, familyTurn)
}

// RecordStep records one graph step keyed by its bounded kind.
func (metrics *BoundedRuntimeMetrics) RecordStep(ctx context.Context, tenantID int64, agentID string, step domain.ExecutionStep, result domain.StepResult, err error) {
	observation := domain.Observation{
		TenantID:  tenantID,
		Component: step.Kind,
		Versions:  domain.ComponentVersions{Release: metrics.config.Release},
		Usage:     result.Usage,
	}
	metrics.finish(&observation, err)
	metrics.record(ctx, observation, familyStep)
}

// RecordDependency records one governed dependency call.
func (metrics *BoundedRuntimeMetrics) RecordDependency(ctx context.Context, tenantID int64, dependency, operation string, usage domain.Usage, err error) {
	observation := domain.Observation{
		TenantID:  tenantID,
		Component: dependency,
		Versions:  domain.ComponentVersions{Release: metrics.config.Release},
		Usage:     usage,
	}
	metrics.finish(&observation, err)
	metrics.record(ctx, observation, familyDependency)
}

// RecordObservation records a stage observation; an empty Stage is a whole-turn observation.
func (metrics *BoundedRuntimeMetrics) RecordObservation(ctx context.Context, observation domain.Observation) {
	if observation.Versions.Release == "" {
		observation.Versions.Release = metrics.config.Release
	}
	family := familyStage
	if observation.Stage == "" {
		family = familyTurn
	}
	metrics.record(ctx, observation, family)
}

type metricFamily int

const (
	familyTurn metricFamily = iota
	familyStage
	familyStep
	familyDependency
)

func (metrics *BoundedRuntimeMetrics) finish(observation *domain.Observation, err error) {
	observation.ErrorClass = stage.ErrorClass(err)
	switch observation.ErrorClass {
	case "":
		observation.Outcome = domain.OutcomeOK
	case "canceled":
		observation.Outcome = domain.OutcomeCanceled
	case "rate_limited", "budget_exceeded", "forbidden", "unauthorized", "validation", "deadline_infeasible", "no_route":
		observation.Outcome = domain.OutcomeRejected
	default:
		observation.Outcome = domain.OutcomeError
	}
}

func (metrics *BoundedRuntimeMetrics) record(ctx context.Context, observation domain.Observation, family metricFamily) {
	if metrics.config.Ledger != nil {
		if err := metrics.config.Ledger.RecordTenantObservation(ctx, observation); err != nil {
			metrics.observe(ctx, MetricLedgerErrors, metrics.labels(observation), 1)
		}
	}
	labels := metrics.labels(observation)
	switch family {
	case familyTurn:
		metrics.observe(ctx, MetricTurnOutcomes, labels, 1)
		if observation.Duration > 0 {
			metrics.observe(ctx, MetricTurnDuration, labels, observation.Duration.Seconds())
		}
	case familyStage:
		metrics.observe(ctx, MetricStageDuration, labels, observation.Duration.Seconds())
		if observation.TimeToFirst > 0 {
			metrics.observe(ctx, MetricTimeToFirstToken, labels, observation.TimeToFirst.Seconds())
		}
		if observation.TimePerOutput > 0 {
			metrics.observe(ctx, MetricTimePerOutputToken, labels, observation.TimePerOutput.Seconds())
		}
	case familyStep:
		metrics.observe(ctx, MetricStepOutcomes, labels, 1)
	case familyDependency:
		metrics.observe(ctx, MetricDependencyCalls, labels, 1)
		if observation.ErrorClass != "" {
			metrics.observe(ctx, MetricDependencyErrors, labels, 1)
		}
	}
	if observation.QueueWait > 0 {
		metrics.observe(ctx, MetricQueueWait, labels, observation.QueueWait.Seconds())
	}
	if observation.Outcome == domain.OutcomeRejected {
		metrics.observe(ctx, MetricAdmissionRejections, labels, 1)
	}
	if observation.ReservedTokens > 0 {
		metrics.observe(ctx, MetricReservationTokenDelta, labels, float64(observation.Usage.InputTokens+observation.Usage.OutputTokens-observation.ReservedTokens))
	}
	metrics.recordUsage(ctx, observation, labels)
}

func (metrics *BoundedRuntimeMetrics) recordUsage(ctx context.Context, observation domain.Observation, labels map[string]string) {
	usage := observation.Usage
	if usage.InputTokens > 0 {
		metrics.observe(ctx, MetricUsageInputTokens, labels, float64(usage.InputTokens))
	}
	if usage.OutputTokens > 0 {
		metrics.observe(ctx, MetricUsageOutputTokens, labels, float64(usage.OutputTokens))
	}
	if usage.ToolCalls > 0 {
		metrics.observe(ctx, MetricUsageToolCalls, labels, float64(usage.ToolCalls))
	}
	if usage.CostMinorUnits > 0 || observation.ReservedCost > 0 {
		withCurrency := make(map[string]string, len(labels)+1)
		for key, value := range labels {
			withCurrency[key] = value
		}
		withCurrency[LabelCurrency] = usage.Currency
		if usage.CostMinorUnits > 0 {
			metrics.observe(ctx, MetricUsageCost, withCurrency, float64(usage.CostMinorUnits))
		}
		if observation.ReservedCost > 0 {
			metrics.observe(ctx, MetricReservationCostDelta, withCurrency, float64(usage.CostMinorUnits-observation.ReservedCost))
		}
	}
}

// labels builds the one fixed label set every catalog series carries.
func (metrics *BoundedRuntimeMetrics) labels(observation domain.Observation) map[string]string {
	stageLabel := string(observation.Stage)
	if stageLabel == "" {
		stageLabel = StageTurn
	}
	release := observation.Versions.Release
	if release == "" {
		release = metrics.config.Release
	}
	return map[string]string{
		LabelTenantTier:    observation.TenantTier,
		LabelPriorityClass: observation.PriorityClass,
		LabelRegion:        observation.Region,
		LabelRelease:       release,
		LabelStage:         stageLabel,
		LabelComponent:     observation.Component,
		LabelModel:         observation.Selection.Model,
		LabelProvider:      observation.Selection.Provider,
		LabelOutcome:       string(observation.Outcome),
		LabelErrorClass:    observation.ErrorClass,
	}
}

// observe drops the sample and counts it when the policy refuses its labels.
func (metrics *BoundedRuntimeMetrics) observe(ctx context.Context, name string, labels map[string]string, value float64) {
	if err := metrics.policy.validate(labels); err != nil {
		metrics.config.Sink.Observe(ctx, MetricLabelRejections, map[string]string{LabelRelease: metrics.config.Release}, 1)
		return
	}
	metrics.config.Sink.Observe(ctx, name, labels, value)
}
