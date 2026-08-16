package modelgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func routeSelection(route string) domain.ModelSelection {
	return domain.ModelSelection{Provider: "p", Model: "m", ModelVersion: "v1", Region: "eu", RouteID: route}
}

func TestServingSignalCollectorAggregatesWindow(t *testing.T) {
	clock := routerNow
	exporter := &fake.ServingSignalExporter{}
	collector, err := NewServingSignalCollector(exporter, time.Minute, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	collector.Now = func() time.Time { return clock }
	collector.Snapshots = &fake.CapacitySnapshotSource{Items: []domain.CapacitySnapshot{
		{Provider: "p", Model: "m", Region: "eu", RouteID: "a", Draining: true, KVPressure: 0.7},
	}}
	ctx := context.Background()

	collector.ObserveServing(ctx, domain.ServingSample{Selection: routeSelection("a"), AdmissionRejected: true, CapacityOutcome: CapacityOutcomeRejected})
	for index, wait := range []time.Duration{10, 20, 30, 40} {
		collector.ObserveServing(ctx, domain.ServingSample{
			Selection: routeSelection("a"), QueueWait: wait * time.Millisecond,
			TimeToFirstToken: time.Duration(index+1) * 100 * time.Millisecond, TimePerOutputToken: 5 * time.Millisecond,
			PrefillTokens: 100, DecodeTokens: 200, CapacityOutcome: CapacityOutcomeGranted,
		})
	}
	collector.ObserveServing(ctx, domain.ServingSample{Selection: routeSelection("b"), CapacityOutcome: CapacityOutcomeCompleted})

	clock = clock.Add(time.Minute)
	if err := collector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(exporter.Signals) != 2 {
		t.Fatalf("signals = %+v", exporter.Signals)
	}
	first := exporter.Signals[0]
	if first.Selection != routeSelection("a") || first.Window != time.Minute || !first.WindowStart.Equal(routerNow) {
		t.Fatalf("signal = %+v", first)
	}
	if first.AdmissionRejections != 1 || first.QueuedPrefillTokens != 400 || first.QueuedDecodeTokenS != 4 {
		t.Fatalf("queued work = %+v", first)
	}
	if first.QueueWaitP50 != 20*time.Millisecond || first.QueueWaitP95 != 40*time.Millisecond ||
		first.TimeToFirstP95 != 400*time.Millisecond || first.TimePerOutputP95 != 5*time.Millisecond {
		t.Fatalf("latency = %+v", first)
	}
	if first.CapacityOutcomes[CapacityOutcomeGranted] != 4 || first.CapacityOutcomes[CapacityOutcomeRejected] != 1 {
		t.Fatalf("outcomes = %+v", first.CapacityOutcomes)
	}
	if !first.Draining || first.KVPressure != 0.7 {
		t.Fatalf("drain state = %+v", first)
	}

	// A window with no samples reports a sampled route once more, then drops it.
	clock = clock.Add(time.Minute)
	if err := collector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(exporter.Signals) != 4 || exporter.Signals[2].QueuedPrefillTokens != 0 || !exporter.Signals[2].Draining {
		t.Fatalf("idle window = %+v", exporter.Signals[2:])
	}
	clock = clock.Add(time.Minute)
	if err := collector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(exporter.Signals) != 4 {
		t.Fatalf("silent routes must stop being exported: %d", len(exporter.Signals))
	}
}

func TestServingSignalCollectorBoundsRoutesAndSamples(t *testing.T) {
	clock := routerNow
	exporter := &fake.ServingSignalExporter{}
	collector, err := NewServingSignalCollector(exporter, time.Minute, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	collector.Now = func() time.Time { return clock }
	ctx := context.Background()
	for _, wait := range []time.Duration{10, 20, 30} {
		collector.ObserveServing(ctx, domain.ServingSample{Selection: routeSelection("a"), QueueWait: wait * time.Millisecond})
	}
	collector.ObserveServing(ctx, domain.ServingSample{Selection: routeSelection("b")})
	collector.ObserveServing(ctx, domain.ServingSample{Selection: domain.ModelSelection{}})
	if collector.Dropped() != 2 {
		t.Fatalf("dropped = %d", collector.Dropped())
	}
	clock = clock.Add(time.Minute)
	if err := collector.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(exporter.Signals) != 1 || exporter.Signals[0].QueueWaitP50 != 20*time.Millisecond {
		t.Fatalf("signals = %+v", exporter.Signals)
	}
}

func TestServingSignalCollectorReportsExportFailures(t *testing.T) {
	exporter := &fake.ServingSignalExporter{Err: errors.New("exporter down")}
	collector, err := NewServingSignalCollector(exporter, time.Minute, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	collector.Now = func() time.Time { return routerNow }
	collector.ObserveServing(context.Background(), domain.ServingSample{Selection: routeSelection("a")})
	if err := collector.Flush(context.Background()); err == nil {
		t.Fatal("expected export failure")
	}
	collector.Snapshots = &fake.CapacitySnapshotSource{Err: errors.New("snapshots down")}
	if err := collector.Flush(context.Background()); err == nil {
		t.Fatal("expected snapshot failure")
	}
}

func TestServingSignalCollectorValidatesConfig(t *testing.T) {
	exporter := &fake.ServingSignalExporter{}
	for _, test := range []struct {
		name       string
		window     time.Duration
		maxRoutes  int
		maxSamples int
	}{
		{name: "zero window", maxRoutes: 1, maxSamples: 1},
		{name: "zero routes", window: time.Minute, maxSamples: 1},
		{name: "zero samples", window: time.Minute, maxRoutes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewServingSignalCollector(exporter, test.window, test.maxRoutes, test.maxSamples); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if _, err := NewServingSignalCollector(nil, time.Minute, 1, 1); err == nil {
		t.Fatal("expected exporter error")
	}
}

func TestPercentileNearestRank(t *testing.T) {
	samples := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second}
	if percentile(nil, 95) != 0 || percentile(samples, 50) != 2*time.Second || percentile(samples, 95) != 4*time.Second {
		t.Fatalf("percentiles = %v %v", percentile(samples, 50), percentile(samples, 95))
	}
}
