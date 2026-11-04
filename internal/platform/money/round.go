// Package money holds small, dependency-free helpers for integer-cents math,
// shared by billing, subscription proration and tax.
//
// The helpers live in a platform package, not a domain package, to avoid
// cross-domain imports. Billing and subscription both depend on money, but
// neither depends on the other.
package money

// RoundHalfToEven computes num/denom rounded to the nearest integer with
// banker's rounding (round half to even). On an exact tie (remainder =
// denom/2), it rounds to the nearest even integer instead of always up.
//
// Half-up rounding adds a systematic positive bias over large batches of
// amounts. Over millions of invoices, that becomes a measurable accounting
// drift that auditors will flag. Half to even averages out to zero bias. The
// IEEE 754 default, Python 3's round(), .NET decimal and most financial and
// GAAP-adjacent systems use this rule.
//
// Requires denom > 0. num may be negative: proration for a downgrade or a
// quantity reduction passes a negative numerator. The rounding works on the
// magnitude and puts the sign back, so negatives round symmetrically (half
// to even on the absolute value). Go's integer division truncates toward
// zero. Used alone, it would round every negative result toward zero and
// understate proration credits by up to 1 cent. Positive numerators are
// unaffected.
//
// The magnitude is worked in uint64, and the remainder is compared with
// denom − remainder rather than doubled. So the result is exact for every
// num (MinInt64 too) and every denom, including one above MaxInt64/2.
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
