package modelgateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// ServingSignalCollector aggregates route-attributed samples into one
// ServingSignal per route per window and hands them to the exporter on Flush.
// A periodic worker owns the flush cadence; the collector runs no goroutines.
type ServingSignalCollector struct {
	Exporter contract.ServingSignalExporter
	// Window is the nominal aggregation period stamped on each signal.
	Window time.Duration
	// MaxRoutes bounds distinct routes per window; samples beyond it are counted as dropped.
	MaxRoutes int
	// MaxSamples bounds retained latency samples per route and metric (last N).
	MaxSamples int
	// Snapshots stamps drain state and KV pressure from the capacity view at flush when set.
	Snapshots contract.CapacitySnapshotSource
	Now       func() time.Time

	mu          sync.Mutex
	windowStart time.Time
	routes      map[routeKey]*routeWindow
	dropped     int64
}

var _ contract.ServingSignalObserver = (*ServingSignalCollector)(nil)

type routeWindow struct {
	selection     domain.ModelSelection
	prefillTokens int64
	decodeTokenS  float64
	lastTPOT      time.Duration
	queueWait     []time.Duration
	timeToFirst   []time.Duration
	timePerOutput []time.Duration
	rejections    int64
	outcomes      map[string]int64
	kvPressure    float64
	pendingDecode int64
	sampled       bool
}

// NewServingSignalCollector validates the window and bounds.
func NewServingSignalCollector(exporter contract.ServingSignalExporter, window time.Duration, maxRoutes, maxSamples int) (*ServingSignalCollector, error) {
	collector := &ServingSignalCollector{Exporter: exporter, Window: window, MaxRoutes: maxRoutes, MaxSamples: maxSamples}
	if err := collector.validate(); err != nil {
		return nil, err
	}
	return collector, nil
}

func (collector *ServingSignalCollector) validate() error {
	if collector.Exporter == nil || collector.Window <= 0 || collector.MaxRoutes <= 0 || collector.MaxSamples <= 0 {
		return fmt.Errorf("serving signal collector: exporter, positive window, max routes, and max samples are required")
	}
	return nil
}

func (collector *ServingSignalCollector) now() time.Time {
	if collector.Now == nil {
		return time.Now()
	}
	return collector.Now()
}

// ObserveServing folds one sample into its route's current window.
func (collector *ServingSignalCollector) ObserveServing(_ context.Context, sample domain.ServingSample) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if collector.validate() != nil || !selectionRouteKey(sample.Selection).valid() {
		collector.dropped++
		return
	}
	if collector.windowStart.IsZero() {
		collector.windowStart = collector.now()
	}
	if collector.routes == nil {
		collector.routes = make(map[routeKey]*routeWindow)
	}
	key := selectionRouteKey(sample.Selection)
	window, ok := collector.routes[key]
	if !ok {
		if len(collector.routes) >= collector.MaxRoutes {
			collector.dropped++
			return
		}
		window = &routeWindow{selection: routeIdentity(sample.Selection), outcomes: make(map[string]int64)}
		collector.routes[key] = window
	}
	window.sampled = true
	if sample.AdmissionRejected {
		window.rejections++
	}
	if sample.CapacityOutcome != "" {
		window.outcomes[sample.CapacityOutcome]++
	}
	window.prefillTokens += max(0, sample.PrefillTokens)
	if sample.TimePerOutputToken > 0 {
		window.lastTPOT = sample.TimePerOutputToken
	}
	// Decode work is token·seconds; the route's latest observed TPOT prices tokens admitted before it is known.
	if window.lastTPOT > 0 {
		window.decodeTokenS += float64(sample.DecodeTokens+window.pendingDecode) * window.lastTPOT.Seconds()
		window.pendingDecode = 0
	} else {
		window.pendingDecode += max(0, sample.DecodeTokens)
	}
	window.queueWait = collector.push(window.queueWait, sample.QueueWait)
	window.timeToFirst = collector.push(window.timeToFirst, sample.TimeToFirstToken)
	window.timePerOutput = collector.push(window.timePerOutput, sample.TimePerOutputToken)
	window.kvPressure = max(window.kvPressure, sample.KVPressure)
}

func (collector *ServingSignalCollector) push(samples []time.Duration, value time.Duration) []time.Duration {
	if value <= 0 {
		return samples
	}
	if len(samples) >= collector.MaxSamples {
		samples = samples[1:]
	}
	return append(samples, value)
}

// Dropped counts samples refused by the route bound or missing route identity.
func (collector *ServingSignalCollector) Dropped() int64 {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.dropped
}

// Flush exports one signal per sampled route and starts a new window; a route
// that saw no sample this window is exported once more with drain state only.
func (collector *ServingSignalCollector) Flush(ctx context.Context) error {
	if err := collector.validate(); err != nil {
		return err
	}
	drains := map[routeKey]domain.CapacitySnapshot{}
	if collector.Snapshots != nil {
		snapshots, err := collector.Snapshots.Snapshots(ctx)
		if err != nil {
			return fmt.Errorf("serving signal snapshots: %w", err)
		}
		for _, snapshot := range snapshots {
			drains[snapshotRouteKey(snapshot)] = snapshot
		}
	}
	collector.mu.Lock()
	now := collector.now()
	start := collector.windowStart
	if start.IsZero() {
		start = now
	}
	keys := make([]routeKey, 0, len(collector.routes))
	for key := range collector.routes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	signals := make([]domain.ServingSignal, 0, len(keys))
	for _, key := range keys {
		window := collector.routes[key]
		signal := domain.ServingSignal{
			Selection:           window.selection,
			WindowStart:         start,
			Window:              now.Sub(start),
			QueuedPrefillTokens: window.prefillTokens,
			QueuedDecodeTokenS:  int64(window.decodeTokenS),
			QueueWaitP50:        percentile(window.queueWait, 50),
			QueueWaitP95:        percentile(window.queueWait, 95),
			TimeToFirstP95:      percentile(window.timeToFirst, 95),
			TimePerOutputP95:    percentile(window.timePerOutput, 95),
			AdmissionRejections: window.rejections,
			CapacityOutcomes:    window.outcomes,
			KVPressure:          window.kvPressure,
		}
		if snapshot, ok := drains[key]; ok {
			signal.Draining = snapshot.Draining
			signal.KVPressure = max(signal.KVPressure, snapshot.KVPressure)
		}
		signals = append(signals, signal)
		if !window.sampled {
			delete(collector.routes, key)
			continue
		}
		collector.routes[key] = &routeWindow{selection: window.selection, lastTPOT: window.lastTPOT, outcomes: make(map[string]int64)}
	}
	collector.windowStart = now
	collector.mu.Unlock()

	var errs []error
	for _, signal := range signals {
		if err := collector.Exporter.Export(ctx, signal); err != nil {
			errs = append(errs, fmt.Errorf("export serving signal for %s: %w", selectionRouteKey(signal.Selection), err))
		}
	}
	return errors.Join(errs...)
}

// routeIdentity keeps only the fields that name a route; pool, generation, and
// reason vary per request and would split the aggregation.
func routeIdentity(selection domain.ModelSelection) domain.ModelSelection {
	return domain.ModelSelection{Provider: selection.Provider, Model: selection.Model, ModelVersion: selection.ModelVersion, Region: selection.Region, RouteID: selection.RouteID}
}

// percentile uses nearest-rank over the retained samples; empty means unobserved.
func percentile(samples []time.Duration, p int) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	rank := (len(sorted)*p + 99) / 100
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}
