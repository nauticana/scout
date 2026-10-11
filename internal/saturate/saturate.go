// Package saturate adds non-negative amounts, such as token counts and minor units, without
// wrapping past math.MaxInt64.
package saturate

import "math"

// Add returns a + b for non-negative a and b, holding at math.MaxInt64.
func Add(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
