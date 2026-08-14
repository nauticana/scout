package isolation

import "time"

// costWindow is a ring of per-slot sums validated by slot epoch.
type costWindow struct {
	sums           []int64
	marks          []int64
	protectedUntil time.Time
}

func (w *costWindow) protect(now time.Time, window time.Duration) {
	until := now.Add(window)
	if until.After(w.protectedUntil) {
		w.protectedUntil = until
	}
}

func newCostWindow(buckets int) *costWindow {
	return &costWindow{sums: make([]int64, buckets), marks: make([]int64, buckets)}
}

func (w *costWindow) add(now time.Time, slot time.Duration, v int64) {
	epoch := now.UnixNano() / int64(slot)
	index := int(epoch % int64(len(w.sums)))
	if w.marks[index] != epoch {
		w.marks[index] = epoch
		w.sums[index] = 0
	}
	w.sums[index] += v
}

func (w *costWindow) total(now time.Time, slot time.Duration) int64 {
	epoch := now.UnixNano() / int64(slot)
	oldest := epoch - int64(len(w.sums)) + 1
	var total int64
	for index, mark := range w.marks {
		if mark >= oldest && mark <= epoch {
			total += w.sums[index]
		}
	}
	return total
}
