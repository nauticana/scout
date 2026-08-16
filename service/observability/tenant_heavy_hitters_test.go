package observability

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func heavyHittersConfig(now func() time.Time) TenantHeavyHittersConfig {
	return TenantHeavyHittersConfig{Width: 1024, Depth: 4, Seed: 11, TopK: 3, Window: time.Minute, Now: now}
}

func TestNewTenantHeavyHittersValidatesConfig(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TenantHeavyHittersConfig)
	}{
		{"zero window", func(c *TenantHeavyHittersConfig) { c.Window = 0 }},
		{"zero width", func(c *TenantHeavyHittersConfig) { c.Width = 0 }},
		{"zero depth", func(c *TenantHeavyHittersConfig) { c.Depth = 0 }},
		{"zero top-k", func(c *TenantHeavyHittersConfig) { c.TopK = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := heavyHittersConfig(nil)
			tc.mutate(&config)
			if _, err := NewTenantHeavyHitters(config); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestTenantHeavyHittersStableRankSlotsUnderChurn(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hitters, err := NewTenantHeavyHitters(heavyHittersConfig(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	var forwarded int
	hitters.config.Next = &fake.ObservationRecorder{RecordObservationFunc: func(context.Context, domain.Observation) { forwarded++ }}
	// 500 distinct tenants churn through; three dominate.
	for tenant := int64(1); tenant <= 500; tenant++ {
		hitters.RecordObservation(context.Background(), domain.Observation{TenantID: tenant})
	}
	for i := 0; i < 50; i++ {
		hitters.Record(7, 1)
	}
	for i := 0; i < 30; i++ {
		hitters.Record(300, 1)
	}
	for i := 0; i < 20; i++ {
		hitters.Record(42, 1)
	}
	if forwarded != 500 {
		t.Fatalf("forwarded = %d", forwarded)
	}
	_, ranked := hitters.Snapshot()
	if len(ranked) != 3 || ranked[0].TenantID != 7 || ranked[1].TenantID != 300 || ranked[2].TenantID != 42 || ranked[0].Rank != 1 || ranked[2].Rank != 3 {
		t.Fatalf("ranked = %+v", ranked)
	}
	if ranked[0].Estimate < 51 || ranked[1].Estimate < 31 || ranked[2].Estimate < 21 {
		t.Fatalf("estimates underestimate: %+v", ranked)
	}

	seen := map[string]bool{}
	sink := &fake.MetricLabelSink{ObserveFunc: func(_ context.Context, name string, labels map[string]string, value float64) {
		if name != MetricTenantRankEstimate || len(labels) != 1 {
			t.Fatalf("unexpected export %s %v", name, labels)
		}
		if _, err := DefaultLabelPolicy().Sanitize(labels); err != nil {
			t.Fatal(err)
		}
		seen[labels[LabelTenantRank]] = true
	}}
	hitters.Export(context.Background(), sink)
	for slot := 1; slot <= 3; slot++ {
		if !seen[strconv.Itoa(slot)] {
			t.Fatalf("slot %d missing from export: %v", slot, seen)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("exported %d slots, want exactly 3", len(seen))
	}

	// After the window rolls the slots still exist but are empty until new weight arrives.
	now = now.Add(2 * time.Minute)
	windowStart, ranked := hitters.Snapshot()
	if len(ranked) != 0 || !windowStart.Equal(now) {
		t.Fatalf("after roll: start %s ranked %+v", windowStart, ranked)
	}
	exports := 0
	hitters.Export(context.Background(), &fake.MetricLabelSink{ObserveFunc: func(_ context.Context, _ string, _ map[string]string, value float64) {
		exports++
		if value != 0 {
			t.Fatalf("empty slot exported %v", value)
		}
	}})
	if exports != 3 {
		t.Fatalf("exports = %d", exports)
	}
}

func TestTenantHeavyHittersMergeAndWeight(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	config := heavyHittersConfig(now)
	config.Weight = func(o domain.Observation) int64 { return o.Usage.OutputTokens }
	a, _ := NewTenantHeavyHitters(config)
	b, _ := NewTenantHeavyHitters(config)
	a.RecordObservation(context.Background(), domain.Observation{TenantID: 1, Usage: domain.Usage{OutputTokens: 10}})
	b.RecordObservation(context.Background(), domain.Observation{TenantID: 1, Usage: domain.Usage{OutputTokens: 15}})
	b.RecordObservation(context.Background(), domain.Observation{TenantID: 2, Usage: domain.Usage{OutputTokens: 20}})
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	_, ranked := a.Snapshot()
	if len(ranked) != 2 || ranked[0].TenantID != 1 || ranked[0].Estimate < 25 || ranked[1].TenantID != 2 || ranked[1].Estimate < 20 {
		t.Fatalf("merged = %+v", ranked)
	}
	config.Seed = 99
	c, _ := NewTenantHeavyHitters(config)
	if err := a.Merge(c); err == nil {
		t.Fatal("incompatible seed merged")
	}
}
