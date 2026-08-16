package observability

import (
	"context"
	"fmt"

	keelport "github.com/nauticana/keel/port"
	"github.com/nauticana/scout/contract"
)

// KeelMetricSink adapts the bounded label sink to keel's MetricsRecorder using
// the catalog for kind and help; gauges and unknown names are reported, not guessed.
type KeelMetricSink struct {
	recorder keelport.MetricsRecorder
	onError  func(error)
}

var _ contract.MetricLabelSink = (*KeelMetricSink)(nil)

// NewKeelMetricSink requires a recorder and an error handler for refused samples.
func NewKeelMetricSink(recorder keelport.MetricsRecorder, onError func(error)) (*KeelMetricSink, error) {
	if recorder == nil || onError == nil {
		return nil, fmt.Errorf("keel metric sink: recorder and error handler are required")
	}
	return &KeelMetricSink{recorder: recorder, onError: onError}, nil
}

// Observe records one catalog sample.
func (sink *KeelMetricSink) Observe(ctx context.Context, name string, labels map[string]string, value float64) {
	metric, ok := LookupMetric(name)
	if !ok {
		sink.onError(fmt.Errorf("keel metric sink: %q is not in the catalog", name))
		return
	}
	var kind keelport.MetricKind
	switch metric.Kind {
	case MetricCounter:
		kind = keelport.MetricCounter
	case MetricHistogram:
		kind = keelport.MetricHistogram
	default:
		sink.onError(fmt.Errorf("keel metric sink: %q kind %s is not supported by the recorder", name, metric.Kind))
		return
	}
	if err := sink.recorder.RecordMetric(ctx, keelport.MetricMeasurement{Name: name, Help: metric.Help, Kind: kind, Value: value, Labels: labels}); err != nil {
		sink.onError(fmt.Errorf("keel metric sink: %w", err))
	}
}
