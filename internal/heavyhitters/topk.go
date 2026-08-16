package heavyhitters

import (
	"container/heap"
	"fmt"
	"sort"
)

// Hitter is one tracked key with its sketch estimate.
type Hitter struct {
	Key      uint64
	Estimate int64
}

// TopK keeps the K largest estimates seen; exactly K rank slots exist
// regardless of how many keys were observed.
type TopK struct {
	k       int
	entries indexedHeap
}

// NewTopK returns an empty tracker; k must be positive.
func NewTopK(k int) (*TopK, error) {
	if k <= 0 {
		return nil, fmt.Errorf("heavy hitters: k must be positive")
	}
	return &TopK{k: k, entries: indexedHeap{index: make(map[uint64]int, k)}}, nil
}

// K is the configured slot count.
func (top *TopK) K() int { return top.k }

// Offer records key's latest estimate, evicting the smallest entry when full.
func (top *TopK) Offer(key uint64, estimate int64) {
	if position, tracked := top.entries.index[key]; tracked {
		top.entries.items[position].Estimate = estimate
		heap.Fix(&top.entries, position)
		return
	}
	if len(top.entries.items) < top.k {
		heap.Push(&top.entries, Hitter{Key: key, Estimate: estimate})
		return
	}
	if estimate <= top.entries.items[0].Estimate {
		return
	}
	heap.Pop(&top.entries)
	heap.Push(&top.entries, Hitter{Key: key, Estimate: estimate})
}

// Ranked returns tracked hitters ordered by estimate descending, key ascending on ties.
func (top *TopK) Ranked() []Hitter {
	ranked := make([]Hitter, len(top.entries.items))
	copy(ranked, top.entries.items)
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Estimate != ranked[j].Estimate {
			return ranked[i].Estimate > ranked[j].Estimate
		}
		return ranked[i].Key < ranked[j].Key
	})
	return ranked
}

// Reset forgets every tracked key.
func (top *TopK) Reset() {
	top.entries.items = top.entries.items[:0]
	clear(top.entries.index)
}

// indexedHeap is a min-heap by estimate that tracks each key's position.
type indexedHeap struct {
	items []Hitter
	index map[uint64]int
}

func (h *indexedHeap) Len() int { return len(h.items) }
func (h *indexedHeap) Less(i, j int) bool {
	if h.items[i].Estimate != h.items[j].Estimate {
		return h.items[i].Estimate < h.items[j].Estimate
	}
	return h.items[i].Key > h.items[j].Key
}
func (h *indexedHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.index[h.items[i].Key] = i
	h.index[h.items[j].Key] = j
}
func (h *indexedHeap) Push(x any) {
	item := x.(Hitter)
	h.index[item.Key] = len(h.items)
	h.items = append(h.items, item)
}
func (h *indexedHeap) Pop() any {
	last := len(h.items) - 1
	item := h.items[last]
	h.items = h.items[:last]
	delete(h.index, item.Key)
	return item
}
