// Package heavyhitters holds the Count-Min sketch and bounded top-K math
// behind Scout's tenant heavy-hitter view; callers provide locking and clock.
package heavyhitters

import (
	"fmt"
	"math"
)

// Sketch is a Count-Min sketch over uint64 keys with a deterministic seed so
// replicas can merge compatible sketches.
type Sketch struct {
	width, depth int
	seed         uint64
	counts       []int64
	total        int64
}

// NewSketch returns an empty sketch; width and depth must be positive.
func NewSketch(width, depth int, seed uint64) (*Sketch, error) {
	if width <= 0 || depth <= 0 {
		return nil, fmt.Errorf("heavy hitters: width and depth must be positive")
	}
	if width > math.MaxInt32 || depth > math.MaxInt32 {
		return nil, fmt.Errorf("heavy hitters: width and depth are too large")
	}
	return &Sketch{width: width, depth: depth, seed: seed, counts: make([]int64, width*depth)}, nil
}

// Add increases key by weight and returns the new estimate.
func (sketch *Sketch) Add(key uint64, weight int64) int64 {
	if weight <= 0 {
		return sketch.Estimate(key)
	}
	sketch.total += weight
	estimate := int64(math.MaxInt64)
	for row := 0; row < sketch.depth; row++ {
		cell := &sketch.counts[row*sketch.width+sketch.column(row, key)]
		*cell += weight
		estimate = min(estimate, *cell)
	}
	return estimate
}

// Estimate returns the point estimate for key; it never underestimates.
func (sketch *Sketch) Estimate(key uint64) int64 {
	estimate := int64(math.MaxInt64)
	for row := 0; row < sketch.depth; row++ {
		estimate = min(estimate, sketch.counts[row*sketch.width+sketch.column(row, key)])
	}
	return estimate
}

// Total is the sum of all added weights.
func (sketch *Sketch) Total() int64 { return sketch.total }

// ErrorBound is the additive error exceeded with probability at most FailureProbability.
func (sketch *Sketch) ErrorBound() int64 {
	return int64(math.Ceil(math.E / float64(sketch.width) * float64(sketch.total)))
}

// FailureProbability is the chance one estimate exceeds ErrorBound.
func (sketch *Sketch) FailureProbability() float64 {
	return math.Exp(-float64(sketch.depth))
}

// Reset clears every counter, keeping the shape and seed.
func (sketch *Sketch) Reset() {
	clear(sketch.counts)
	sketch.total = 0
}

// Merge adds a compatible sketch's counters into this one.
func (sketch *Sketch) Merge(other *Sketch) error {
	if other == nil || other.width != sketch.width || other.depth != sketch.depth || other.seed != sketch.seed {
		return fmt.Errorf("heavy hitters: sketches have incompatible width, depth, or seed")
	}
	for i := range sketch.counts {
		sketch.counts[i] += other.counts[i]
	}
	sketch.total += other.total
	return nil
}

func (sketch *Sketch) column(row int, key uint64) int {
	return int(mix(sketch.seed^(uint64(row)+1)*0x9E3779B97F4A7C15^key) % uint64(sketch.width))
}

// mix is the splitmix64 finalizer, a cheap deterministic 64-bit hash.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x
}
