package saturate

import (
	"math"
	"testing"
)

func TestAddHoldsAtTheMaximum(t *testing.T) {
	for _, test := range []struct{ a, b, want int64 }{
		{2, 3, 5}, {0, 0, 0}, {math.MaxInt64, 0, math.MaxInt64}, {math.MaxInt64 - 1, 1, math.MaxInt64}, {math.MaxInt64, math.MaxInt64, math.MaxInt64},
	} {
		if got := Add(test.a, test.b); got != test.want {
			t.Errorf("Add(%d, %d) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}
