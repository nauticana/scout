package observability

import (
	"context"
	"testing"

	keelport "github.com/nauticana/keel/port"
)

type keelRecorderFunc func(context.Context, keelport.MetricMeasurement) error

func (f keelRecorderFunc) RecordMetric(ctx context.Context, m keelport.MetricMeasurement) error {
	return f(ctx, m)
}

func TestKeelMetricSinkMapsCatalogKinds(t *testing.T) {
	var recorded []keelport.MetricMeasurement
	var failures []error
	sink, err := NewKeelMetricSink(keelRecorderFunc(func(_ context.Context, m keelport.MetricMeasurement) error {
		recorded = append(recorded, m)
		return nil
	}), func(err error) { failures = append(failures, err) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewKeelMetricSink(nil, nil); err == nil {
		t.Fatal("nil recorder accepted")
	}
	labels := map[string]string{LabelStage: "model"}
	sink.Observe(context.Background(), MetricTurnOutcomes, labels, 1)
	sink.Observe(context.Background(), MetricStageDuration, labels, 0.5)
	sink.Observe(context.Background(), MetricTenantRankEstimate, map[string]string{LabelTenantRank: "1"}, 3)
	sink.Observe(context.Background(), "scout_not_in_catalog", labels, 1)
	if len(recorded) != 2 || recorded[0].Kind != keelport.MetricCounter || recorded[1].Kind != keelport.MetricHistogram || recorded[1].Help == "" || recorded[1].Labels[LabelStage] != "model" {
		t.Fatalf("recorded = %+v", recorded)
	}
	if len(failures) != 2 {
		t.Fatalf("failures = %v", failures)
	}
}
