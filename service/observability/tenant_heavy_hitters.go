package observability

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/heavyhitters"
)

// TenantHeavyHittersConfig configures the sketch, the rank slots, and the window.
type TenantHeavyHittersConfig struct {
	// Width and Depth shape the Count-Min sketch: error e/Width of the window total with probability 1-e^-Depth.
	Width, Depth int
	// Seed must match across replicas whose sketches are merged.
	Seed uint64
	// TopK is the number of stable rank slots exported.
	TopK int
	// Window resets the sketch and ranks; must be positive.
	Window time.Duration
	// Weight scores one observation; nil counts observations.
	Weight func(domain.Observation) int64
	// Next receives every observation unchanged; nil ends the chain.
	Next contract.ObservationRecorder
	Now  func() time.Time
}

// TenantHeavyHitter is one resolved rank slot; identity leaves only through Snapshot.
type TenantHeavyHitter struct {
	Rank     int
	TenantID int64
	Estimate int64
}

// TenantHeavyHitters ranks tenants by sketched weight inside a rolling window
// and exports exactly TopK rank slots so churn never adds series.
type TenantHeavyHitters struct {
	config      TenantHeavyHittersConfig
	mu          sync.Mutex
	sketch      *heavyhitters.Sketch
	top         *heavyhitters.TopK
	windowStart time.Time
}

var _ contract.ObservationRecorder = (*TenantHeavyHitters)(nil)

// NewTenantHeavyHitters validates the config and builds an empty window.
func NewTenantHeavyHitters(config TenantHeavyHittersConfig) (*TenantHeavyHitters, error) {
	if config.Window <= 0 {
		return nil, fmt.Errorf("tenant heavy hitters: window must be positive")
	}
	sketch, err := heavyhitters.NewSketch(config.Width, config.Depth, config.Seed)
	if err != nil {
		return nil, fmt.Errorf("tenant heavy hitters: %w", err)
	}
	top, err := heavyhitters.NewTopK(config.TopK)
	if err != nil {
		return nil, fmt.Errorf("tenant heavy hitters: %w", err)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	hitters := &TenantHeavyHitters{config: config, sketch: sketch, top: top}
	hitters.windowStart = config.Now()
	return hitters, nil
}

// RecordObservation adds the observation's weight for its tenant and forwards to Next.
func (hitters *TenantHeavyHitters) RecordObservation(ctx context.Context, observation domain.Observation) {
	if observation.TenantID > 0 {
		weight := int64(1)
		if hitters.config.Weight != nil {
			weight = hitters.config.Weight(observation)
		}
		hitters.Record(observation.TenantID, weight)
	}
	if hitters.config.Next != nil {
		hitters.config.Next.RecordObservation(ctx, observation)
	}
}

// Record adds weight for tenantID inside the current window.
func (hitters *TenantHeavyHitters) Record(tenantID int64, weight int64) {
	if tenantID <= 0 || weight <= 0 {
		return
	}
	hitters.mu.Lock()
	defer hitters.mu.Unlock()
	hitters.rollLocked(hitters.config.Now())
	estimate := hitters.sketch.Add(uint64(tenantID), weight)
	hitters.top.Offer(uint64(tenantID), estimate)
}

// Snapshot resolves rank slots to tenant identity for a protected diagnostics path.
func (hitters *TenantHeavyHitters) Snapshot() (windowStart time.Time, ranked []TenantHeavyHitter) {
	hitters.mu.Lock()
	defer hitters.mu.Unlock()
	hitters.rollLocked(hitters.config.Now())
	for i, hitter := range hitters.top.Ranked() {
		ranked = append(ranked, TenantHeavyHitter{Rank: i + 1, TenantID: int64(hitter.Key), Estimate: hitter.Estimate})
	}
	return hitters.windowStart, ranked
}

// Export writes MetricTenantRankEstimate for every slot 1..TopK; empty slots export zero.
func (hitters *TenantHeavyHitters) Export(ctx context.Context, sink contract.MetricLabelSink) {
	_, ranked := hitters.Snapshot()
	for slot := 1; slot <= hitters.config.TopK; slot++ {
		var estimate int64
		if slot <= len(ranked) {
			estimate = ranked[slot-1].Estimate
		}
		sink.Observe(ctx, MetricTenantRankEstimate, map[string]string{LabelTenantRank: strconv.Itoa(slot)}, float64(estimate))
	}
}

// Merge folds a replica's sketch into this one; ranks are re-derived from merged estimates.
func (hitters *TenantHeavyHitters) Merge(other *TenantHeavyHitters) error {
	if other == nil {
		return fmt.Errorf("tenant heavy hitters: nil peer")
	}
	other.mu.Lock()
	peerSketch := other.sketch
	peerRanked := other.top.Ranked()
	other.mu.Unlock()
	hitters.mu.Lock()
	defer hitters.mu.Unlock()
	if err := hitters.sketch.Merge(peerSketch); err != nil {
		return err
	}
	for _, hitter := range peerRanked {
		hitters.top.Offer(hitter.Key, hitters.sketch.Estimate(hitter.Key))
	}
	for _, hitter := range hitters.top.Ranked() {
		hitters.top.Offer(hitter.Key, hitters.sketch.Estimate(hitter.Key))
	}
	return nil
}

func (hitters *TenantHeavyHitters) rollLocked(now time.Time) {
	if now.Sub(hitters.windowStart) < hitters.config.Window {
		return
	}
	hitters.sketch.Reset()
	hitters.top.Reset()
	hitters.windowStart = now
}
