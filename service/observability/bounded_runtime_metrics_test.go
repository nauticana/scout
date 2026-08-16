package observability

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

type sample struct {
	name   string
	labels map[string]string
	value  float64
}

type sampleSink struct{ samples []sample }

func (sink *sampleSink) fake() *fake.MetricLabelSink {
	return &fake.MetricLabelSink{ObserveFunc: func(_ context.Context, name string, labels map[string]string, value float64) {
		sink.samples = append(sink.samples, sample{name, labels, value})
	}}
}

func (sink *sampleSink) named(name string) []sample {
	var out []sample
	for _, s := range sink.samples {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

func TestNewBoundedRuntimeMetricsValidatesConfig(t *testing.T) {
	if _, err := NewBoundedRuntimeMetrics(BoundedRuntimeMetricsConfig{}); err == nil {
		t.Fatal("nil sink accepted")
	}
	sink := &sampleSink{}
	if _, err := NewBoundedRuntimeMetrics(BoundedRuntimeMetricsConfig{Sink: sink.fake(), Release: "release with spaces"}); err == nil {
		t.Fatal("free-text release accepted")
	}
	if _, err := NewBoundedRuntimeMetrics(BoundedRuntimeMetricsConfig{Sink: sink.fake(), Policy: LabelPolicy{Allowed: []string{LabelTenantID}}}); err == nil {
		t.Fatal("policy allowing tenant_id accepted")
	}
}

func TestBoundedRuntimeMetricsNeverLabelsTenantIdentity(t *testing.T) {
	sink := &sampleSink{}
	var ledgered []domain.Observation
	metrics, err := NewBoundedRuntimeMetrics(BoundedRuntimeMetricsConfig{
		Sink:    sink.fake(),
		Release: "v1.2.3",
		Ledger:  &fake.TenantLedger{RecordTenantObservationFunc: func(_ context.Context, o domain.Observation) error { ledgered = append(ledgered, o); return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 42, Tier: "gold", PriorityClass: "interactive", Region: "eu"}, RequestID: "req-1", ConversationID: "conv-1"}
	result := domain.TurnResult{AgentVersion: "a7", Usage: domain.Usage{InputTokens: 10, OutputTokens: 5, ToolCalls: 1, CostMinorUnits: 30, Currency: "USD"}}
	metrics.RecordTurn(context.Background(), request, result, nil)

	if len(sink.samples) == 0 {
		t.Fatal("no samples")
	}
	for _, s := range sink.samples {
		for key, value := range s.labels {
			if key == LabelTenantID || key == LabelRequestID || key == LabelConversationID || value == "42" || value == "req-1" || value == "conv-1" {
				t.Fatalf("identity leaked into %s labels %v", s.name, s.labels)
			}
		}
		if _, err := DefaultLabelPolicy().Sanitize(s.labels); err != nil {
			t.Fatalf("%s labels violate policy: %v", s.name, err)
		}
	}
	outcomes := sink.named(MetricTurnOutcomes)
	if len(outcomes) != 1 || outcomes[0].labels[LabelTenantTier] != "gold" || outcomes[0].labels[LabelPriorityClass] != "interactive" ||
		outcomes[0].labels[LabelRegion] != "eu" || outcomes[0].labels[LabelRelease] != "v1.2.3" || outcomes[0].labels[LabelOutcome] != "ok" || outcomes[0].labels[LabelStage] != StageTurn {
		t.Fatalf("turn outcomes = %+v", outcomes)
	}
	if cost := sink.named(MetricUsageCost); len(cost) != 1 || cost[0].value != 30 || cost[0].labels[LabelCurrency] != "USD" {
		t.Fatalf("cost = %+v", cost)
	}
	if in := sink.named(MetricUsageInputTokens); len(in) != 1 || in[0].value != 10 {
		t.Fatalf("input tokens = %+v", in)
	}
	if len(ledgered) != 1 || ledgered[0].TenantID != 42 || ledgered[0].Component != ComponentTurn || ledgered[0].Versions.Agent != "a7" || ledgered[0].Versions.Release != "v1.2.3" {
		t.Fatalf("ledger = %+v", ledgered)
	}
}

func TestBoundedRuntimeMetricsOutcomesAndRejections(t *testing.T) {
	sink := &sampleSink{}
	metrics, _ := NewBoundedRuntimeMetrics(BoundedRuntimeMetricsConfig{Sink: sink.fake(), Release: "r"})
	turn := domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 1, Tier: "silver"}}
	metrics.RecordTurn(context.Background(), turn, domain.TurnResult{}, domain.ErrRateLimited)
	metrics.RecordTurn(context.Background(), turn, domain.TurnResult{}, context.Canceled)
	metrics.RecordTurn(context.Background(), turn, domain.TurnResult{}, errors.New("boom"))

	outcomes := sink.named(MetricTurnOutcomes)
	if len(outcomes) != 3 || outcomes[0].labels[LabelOutcome] != "rejected" || outcomes[0].labels[LabelErrorClass] != "rate_limited" ||
		outcomes[1].labels[LabelOutcome] != "canceled" || outcomes[2].labels[LabelOutcome] != "error" || outcomes[2].labels[LabelErrorClass] != "internal" {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	if rejected := sink.named(MetricAdmissionRejections); len(rejected) != 1 || rejected[0].labels[LabelErrorClass] != "rate_limited" {
		t.Fatalf("rejections = %+v", rejected)
	}

	metrics.RecordDependency(context.Background(), 1, "hot_session_cache", "get", domain.Usage{}, domain.ErrCircuitOpen)
	metrics.RecordDependency(context.Background(), 1, "hot_session_cache", "get", domain.Usage{}, nil)
	if calls, failures := sink.named(MetricDependencyCalls), sink.named(MetricDependencyErrors); len(calls) != 2 || len(failures) != 1 ||
		failures[0].labels[LabelComponent] != "hot_session_cache" || failures[0].labels[LabelErrorClass] != "circuit_open" {
		t.Fatalf("dependency calls=%+v errors=%+v", calls, failures)
	}

	metrics.RecordStep(context.Background(), 1, "agent", domain.ExecutionStep{Kind: "tool"}, domain.StepResult{Usage: domain.Usage{ToolCalls: 2}}, nil)
	if steps := sink.named(MetricStepOutcomes); len(steps) != 1 || steps[0].labels[LabelComponent] != "tool" {
		t.Fatalf("steps = %+v", steps)
	}
	if tools := sink.named(MetricUsageToolCalls); len(tools) != 1 || tools[0].value != 2 {
		t.Fatalf("tool calls = %+v", tools)
	}
}

func TestBoundedRuntimeMetricsRecordsStageObservations(t *testing.T) {
	sink := &sampleSink{}
	metrics, _ := NewBoundedRuntimeMetrics(BoundedRuntimeMetricsConfig{Sink: sink.fake(), Release: "r"})
	metrics.RecordObservation(context.Background(), domain.Observation{
		TenantID:       9,
		TenantTier:     "gold",
		Stage:          domain.StageModel,
		Component:      "stream_pump",
		Selection:      domain.ModelSelection{Provider: "anthropic", Model: "claude-sonnet"},
		Duration:       2 * time.Second,
		QueueWait:      100 * time.Millisecond,
		TimeToFirst:    300 * time.Millisecond,
		TimePerOutput:  20 * time.Millisecond,
		Usage:          domain.Usage{OutputTokens: 40, InputTokens: 100, CostMinorUnits: 12, Currency: "EUR"},
		ReservedTokens: 200,
		ReservedCost:   20,
		Outcome:        domain.OutcomeOK,
	})
	expect := map[string]float64{
		MetricStageDuration:         2,
		MetricTimeToFirstToken:      0.3,
		MetricTimePerOutputToken:    0.02,
		MetricQueueWait:             0.1,
		MetricReservationTokenDelta: -60,
		MetricReservationCostDelta:  -8,
	}
	for name, want := range expect {
		got := sink.named(name)
		if len(got) != 1 || got[0].value != want {
			t.Fatalf("%s = %+v, want %v", name, got, want)
		}
		if got[0].labels[LabelStage] != "model" || got[0].labels[LabelModel] != "claude-sonnet" || got[0].labels[LabelProvider] != "anthropic" || got[0].labels[LabelRelease] != "r" {
			t.Fatalf("%s labels = %v", name, got[0].labels)
		}
	}
	if len(sink.named(MetricTurnDuration)) != 0 {
		t.Fatal("stage observation recorded as turn")
	}

	metrics.RecordObservation(context.Background(), domain.Observation{TenantID: 9, Component: ComponentTurn, Duration: time.Second, Outcome: domain.OutcomeOK})
	if turn := sink.named(MetricTurnDuration); len(turn) != 1 || turn[0].value != 1 || turn[0].labels[LabelStage] != StageTurn {
		t.Fatalf("turn duration = %+v", turn)
	}
}

func TestBoundedRuntimeMetricsDropsPolicyViolationsLoudly(t *testing.T) {
	sink := &sampleSink{}
	metrics, _ := NewBoundedRuntimeMetrics(BoundedRuntimeMetricsConfig{Sink: sink.fake(), Release: "r"})
	metrics.RecordObservation(context.Background(), domain.Observation{TenantID: 1, Stage: domain.StageModel, Component: "free text component", Outcome: domain.OutcomeOK})
	if len(sink.named(MetricStageDuration)) != 0 {
		t.Fatal("violating labels were exported")
	}
	if rejected := sink.named(MetricLabelRejections); len(rejected) != 1 || rejected[0].value != 1 {
		t.Fatalf("label rejections = %+v", rejected)
	}
}

func TestBoundedRuntimeMetricsCountsLedgerFailures(t *testing.T) {
	sink := &sampleSink{}
	metrics, _ := NewBoundedRuntimeMetrics(BoundedRuntimeMetricsConfig{
		Sink:   sink.fake(),
		Ledger: &fake.TenantLedger{RecordTenantObservationFunc: func(context.Context, domain.Observation) error { return errors.New("db down") }},
	})
	metrics.RecordTurn(context.Background(), domain.TurnRequest{TenantContext: domain.TenantContext{TenantID: 1}}, domain.TurnResult{}, nil)
	if failures := sink.named(MetricLedgerErrors); len(failures) != 1 {
		t.Fatalf("ledger errors = %+v", failures)
	}
	if len(sink.named(MetricTurnOutcomes)) != 1 {
		t.Fatal("ledger failure suppressed fleet series")
	}
}
