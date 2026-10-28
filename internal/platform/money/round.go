// Package money holds small, dependency-free helpers for integer-cents math
// shared across billing, subscription proration, and tax.
//
// Keeping these helpers in a platform package (not a domain package) avoids
// cross-domain imports: billing and subscription both depend on money, but
// neither depends on the other.
package money

// RoundHalfToEven computes num/denom rounded to the nearest integer using
// banker's rounding (round half to even). On exact ties (remainder = denom/2),
// rounds toward the nearest even integer instead of always rounding up.
//
// Why not half-up? Half-up introduces a systematic positive bias when rounding
// large batches of monetary amounts — over millions of invoices, this becomes
// a measurable accounting drift that auditors will flag. Half-to-even averages
// out to zero bias. IEEE 754 default, Python 3's round(), .NET decimal, and
// most financial/GAAP-adjacent systems use this rule.
//
// Requires denom > 0. num may be negative — downgrade/quantity-reduction
// proration passes a negative numerator. The rounding operates on the
// magnitude and reapplies the sign so negatives round symmetrically (half to
// even on the absolute value). Without this, Go's truncate-toward-zero integer
// division rounded every negative result toward zero, understating proration
// credits by up to 1 cent. Positive numerators are unaffected.
//
// The magnitude is worked in uint64, and the remainder is compared with
// denom − remainder rather than doubled, so the result is exact for every
// num (MinInt64 too) and every denom, also one above MaxInt64/2.
func RoundHalfToEven(num, denom int64) int64 {
	neg := num < 0
	d := uint64(denom)
	q, r := abs64(num)/d, abs64(num)%d
	if r > d-r || (r == d-r && q%2 == 1) {
		q++ // q < 2^62 here: denom is at least 2, as r > 0
	}
	if neg {
		return -int64(q) // q is at most 2^63, whose negation is MinInt64
	}
	return int64(q)
}
