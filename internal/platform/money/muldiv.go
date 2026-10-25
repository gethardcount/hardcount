package money

import (
	"math"
	"math/bits"
)

// MulDivRem returns a×b / d as a quotient and remainder, with the product
// worked out in 128 bits, so it is exact whenever the quotient fits int64.
// a×b of two int64 amounts overflows int64 long before a×b/d does: a tax of
// base × rate-in-ppm / 10^6 wraps for a base above about 9.2×10^12 cents.
//
// Requires a, b >= 0 and d > 0. ok is false when the quotient does not fit
// int64 (or an argument is out of range); q and r are then 0.
func MulDivRem(a, b, d int64) (q, r int64, ok bool) {
	if a < 0 || b < 0 || d <= 0 {
		return 0, 0, false
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi >= uint64(d) {
		return 0, 0, false // the quotient needs more than 64 bits
	}
	uq, ur := bits.Div64(hi, lo, uint64(d))
	if uq > math.MaxInt64 {
		return 0, 0, false
	}
	return int64(uq), int64(ur), true
}

// MulDivRoundHalfToEven returns a×b / d rounded half to even (see
// RoundHalfToEven), with the product worked out in 128 bits. Requires
// a, b >= 0 and d > 0; ok is false when the result does not fit int64.
func MulDivRoundHalfToEven(a, b, d int64) (int64, bool) {
	q, r, ok := MulDivRem(a, b, d)
	if !ok {
		return 0, false
	}
	// r < d <= MaxInt64, so 2r fits uint64.
	doubled := 2 * uint64(r)
	if doubled > uint64(d) || (doubled == uint64(d) && q%2 == 1) {
		if q == math.MaxInt64 {
			return 0, false
		}
		q++
	}
	return q, true
}

// MulDivRoundHalfToEvenSigned is MulDivRoundHalfToEven for a and b of any
// sign: |a×b| / d rounded half to even, with the sign of a×b put back, so
// negatives round symmetrically as RoundHalfToEven's do. Wherever a×b fits
// int64 and is not MinInt64 the result is RoundHalfToEven(a*b, d). Requires
// d > 0; ok is false when the result does not fit int64 or is MinInt64 (the
// range is symmetric).
func MulDivRoundHalfToEvenSigned(a, b, d int64) (int64, bool) {
	if d <= 0 {
		return 0, false
	}
	neg := (a < 0) != (b < 0)
	hi, lo := bits.Mul64(abs64(a), abs64(b))
	if hi >= uint64(d) {
		return 0, false
	}
	uq, ur := bits.Div64(hi, lo, uint64(d))
	if uq > math.MaxInt64 {
		return 0, false
	}
	// ur < d <= MaxInt64, so 2ur fits uint64.
	if doubled := 2 * ur; doubled > uint64(d) || (doubled == uint64(d) && uq%2 == 1) {
		if uq == math.MaxInt64 {
			return 0, false
		}
		uq++
	}
	if neg {
		return -int64(uq), true
	}
	return int64(uq), true
}

// Add returns a + b for amounts of any sign; ok is false when the sum does
// not fit int64.
func Add(a, b int64) (int64, bool) {
	c := a + b
	if (a > 0 && b > 0 && c < 0) || (a < 0 && b < 0 && c >= 0) {
		return 0, false
	}
	return c, true
}

// Mul returns a × b for amounts of any sign; ok is false when the product
// does not fit int64.
func Mul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	c := a * b
	if c/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, false
	}
	return c, true
}

// abs64 is |a| as a uint64, which holds |MinInt64| too.
func abs64(a int64) uint64 {
	if a < 0 {
		return uint64(-(a + 1)) + 1
	}
	return uint64(a)
}
