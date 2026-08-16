package heavyhitters

import (
	"math/rand"
	"testing"
)

func TestSketchNeverUnderestimatesAndHonorsErrorBound(t *testing.T) {
	sketch, err := NewSketch(2048, 4, 42)
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(1))
	// Zipf skew: a few tenants dominate.
	zipf := rand.NewZipf(random, 1.2, 1, 5000)
	exact := make(map[uint64]int64)
	for i := 0; i < 50000; i++ {
		key := zipf.Uint64()
		weight := int64(1 + random.Intn(3))
		exact[key] += weight
		sketch.Add(key, weight)
	}
	bound := sketch.ErrorBound()
	violations := 0
	for key, want := range exact {
		got := sketch.Estimate(key)
		if got < want {
			t.Fatalf("key %d estimate %d < exact %d", key, got, want)
		}
		if got-want > bound {
			violations++
		}
	}
	if allowed := int(float64(len(exact)) * sketch.FailureProbability()); violations > allowed {
		t.Fatalf("%d of %d keys exceed error bound %d (allowed %d)", violations, len(exact), bound, allowed)
	}
	if sketch.Estimate(1<<60) > bound {
		t.Fatalf("unseen key estimate %d exceeds bound %d", sketch.Estimate(1<<60), bound)
	}
}

func TestSketchMergeRequiresCompatibleShape(t *testing.T) {
	a, _ := NewSketch(64, 3, 7)
	b, _ := NewSketch(64, 3, 7)
	other, _ := NewSketch(64, 3, 8)
	a.Add(1, 5)
	b.Add(1, 4)
	if err := a.Merge(b); err != nil || a.Estimate(1) != 9 || a.Total() != 9 {
		t.Fatalf("merge = %v, estimate %d", err, a.Estimate(1))
	}
	if err := a.Merge(other); err == nil {
		t.Fatal("expected seed mismatch to be refused")
	}
	if _, err := NewSketch(0, 1, 0); err == nil {
		t.Fatal("expected invalid width to be refused")
	}
	a.Reset()
	if a.Estimate(1) != 0 || a.Total() != 0 {
		t.Fatal("reset left counts")
	}
}

func TestTopKKeepsLargestAndUpdatesInPlace(t *testing.T) {
	top, err := NewTopK(3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTopK(0); err == nil {
		t.Fatal("expected invalid k to be refused")
	}
	for key := uint64(1); key <= 10; key++ {
		top.Offer(key, int64(key))
	}
	ranked := top.Ranked()
	if len(ranked) != 3 || ranked[0].Key != 10 || ranked[1].Key != 9 || ranked[2].Key != 8 {
		t.Fatalf("ranked = %+v", ranked)
	}
	top.Offer(8, 50)
	top.Offer(2, 20)
	ranked = top.Ranked()
	if ranked[0].Key != 8 || ranked[0].Estimate != 50 || ranked[1].Key != 2 || ranked[2].Key != 10 {
		t.Fatalf("ranked = %+v", ranked)
	}
	// Ties break by key ascending so exports are deterministic.
	top.Offer(1, 50)
	ranked = top.Ranked()
	if ranked[0].Key != 1 || ranked[1].Key != 8 {
		t.Fatalf("tie order = %+v", ranked)
	}
	top.Reset()
	if len(top.Ranked()) != 0 {
		t.Fatal("reset left entries")
	}
}
