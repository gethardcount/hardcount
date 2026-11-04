package domain

import (
	"math"
	"strconv"
	"strings"

	"github.com/gethardcount/hardcount/internal/platform/money"
)

// MulCents multiplies two non-negative minor-unit amounts, checked for
// overflow. MulCents and AddCents return ErrAmountOverflow where int64
// multiplication and addition would wrap silently, and the caller refuses the
// request. A wrapped result is dangerous: a credit-note line of 3e9 × 4e9
// cents wraps to a negative total, which then passes the "not more than the
// invoice" cap.
func MulCents(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, ErrAmountOverflow
	}
	if a != 0 && b > math.MaxInt64/a {
		return 0, ErrAmountOverflow
	}
	return a * b, nil
}

// AddCents adds two non-negative minor-unit amounts, checked for overflow as
// MulCents describes.
func AddCents(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, ErrAmountOverflow
	}
	return a + b, nil
}

// SumCents adds two minor-unit amounts of any sign (a ledger entry, a credit
// back). It returns ErrAmountOverflow when the sum does not fit int64. Inside
// int64 it is a + b.
func SumCents(a, b int64) (int64, error) {
	c, ok := money.Add(a, b)
	if !ok {
		return 0, ErrAmountOverflow
	}
	return c, nil
}

// FormatMoneyMinor renders a minor-unit amount for operator-facing text:
// FormatMoneyMinor(10000000, "usd") == "100,000.00 USD". It makes three
// deliberate choices:
//
//   - The ISO code, never a bare symbol. A hardcoded "$" mislabels the money
//     of every tenant that does not bill in USD. ADR-100 makes one currency
//     per meter, plan, subscription and customer a structural invariant, and
//     showing the code is the cheap half of keeping that true.
//   - Integer math, not float64. The amounts here are commit grants, which
//     run to six and seven figures. Past 2^53 minor units a float64 division
//     is no longer exact. A displayed balance that the engine rounded is a
//     number that reconciles against nothing.
//   - Thousands separators. The commit exposure an operator sees routinely
//     shows amounts above five digits. A person misreads "10000000.00 USD"
//     by an order of magnitude at a glance, which defeats the point of
//     showing it.
//
// Minor units are assumed to be hundredths, as in every int64 cents field in
// the schema. A zero-decimal currency (JPY, KRW) would need a table of
// currency exponents. Guessing an exponent here instead of looking one up
// would be a silent fallback, which the project bans. Hardcount has no
// zero-decimal tenant today. When one arrives, this is the single place that
// changes.
func FormatMoneyMinor(cents int64, currency string) string {
	code := strings.ToUpper(strings.TrimSpace(currency))

	neg := cents < 0
	// Negate in uint64 space: -math.MinInt64 overflows int64, and a balance
	// this function cannot render correctly must not render a wrong one.
	var abs uint64
	if neg {
		abs = uint64(-(cents + 1)) + 1
	} else {
		abs = uint64(cents)
	}

	whole := strconv.FormatUint(abs/100, 10)
	frac := abs % 100

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	// Group the integer part in threes from the right.
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	b.WriteByte('.')
	if frac < 10 {
		b.WriteByte('0')
	}
	b.WriteString(strconv.FormatUint(frac, 10))

	if code != "" {
		b.WriteByte(' ')
		b.WriteString(code)
	}
	return b.String()
}
