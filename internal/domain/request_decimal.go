package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/errs"
)

// Every decimal a request sends (a usage quantity, a rate, a provider cost, a
// usage threshold) is read with ParseDecimal or ParseDecimalString and checked
// with CheckDecimal, so each field's limits are stated once and hold at every
// door.

// DecimalLimits is the range a request decimal must fall in.
type DecimalLimits struct {
	// IntDigits: the magnitude is under 10^IntDigits.
	IntDigits int64
	// Scale: at most Scale decimal places (trailing zeros do not count).
	Scale int64
}

var (
	// QuantityLimits fit the NUMERIC(38,12) quantity columns: usage
	// quantities and item usage thresholds.
	QuantityLimits = DecimalLimits{IntDigits: 26, Scale: 12}
	// RateLimits bound a price amount in cents: a per-unit rate, a tier
	// rate or a package amount. A rate under 10^18 cents prices one unit
	// (or one package) inside int64 cents, the type every invoice amount
	// is held in; 12 places is Stripe's unit_amount_decimal precision.
	// The limit does not bound what a rate bills: rate × quantity, the
	// invoice's sum and its total with tax are each refused with
	// ErrAmountOverflow when they do not fit int64 (ComputeAmountCents,
	// billing.ApplyTaxToLineItems).
	RateLimits = DecimalLimits{IntDigits: 18, Scale: 12}
	// ProviderCostLimits fit provider_cost_rates.unit_cost_cents,
	// NUMERIC(22,12) (cents per token).
	ProviderCostLimits = DecimalLimits{IntDigits: 10, Scale: 12}
)

// MaxDecimalChars is the longest text a request decimal may be written in.
// Any value inside the limits above, written plainly, is under 45
// characters; only padding with zeros reaches 100.
const MaxDecimalChars = 100

// CheckDecimal refuses a value outside lim and returns the value to store.
//
// The value is coefficient × 10^exponent, and both limits are decided from
// the coefficient's digit count and the exponent before any comparison or
// rounding. A zero passes
// both limits whatever its exponent, so it is returned as plain 0.
func CheckDecimal(field string, d decimal.Decimal, lim DecimalLimits) (decimal.Decimal, error) {
	if d.IsZero() {
		return decimal.New(0, 0), nil
	}
	digits, exp := coefficientDigits(d), int64(d.Exponent())
	// 10^(digits+exp-1) <= |d| < 10^(digits+exp), so |d| < 10^IntDigits
	// exactly when digits+exp <= IntDigits.
	if digits+exp > lim.IntDigits {
		return decimal.Decimal{}, errs.Invalid(field, fmt.Sprintf("%s is too large: it must be under 10^%d", field, lim.IntDigits))
	}
	// At most digits-1 trailing zeros can come off, so below this the value
	// has more than Scale decimal places whatever its digits are. Above it
	// the rounding works on at most `digits` more digits.
	if digits-1+exp < -lim.Scale || !d.Equal(d.Round(int32(lim.Scale))) {
		return decimal.Decimal{}, errs.Invalid(field, fmt.Sprintf("%s has more than %d decimal places", field, lim.Scale))
	}
	return d, nil
}

// CheckRate checks a price amount in cents (a plan price's or a price
// override's unit_amount_cents, tier unit_amount_cents and
// package_amount_cents): inside RateLimits and 0 or more. A negative
// rate is a discount misspelled; credits are the discount primitive
// (ADR-039).
func CheckRate(field string, d decimal.Decimal) (decimal.Decimal, error) {
	d, err := CheckDecimal(field, d, RateLimits)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if d.IsNegative() {
		return decimal.Decimal{}, errs.Invalid(field, "unit price must not be negative (use 0 for a free rate)")
	}
	return d, nil
}

// coefficientDigits counts the decimal digits of d's coefficient exactly.
// shopspring's NumDigits takes a float log10 for a coefficient up to 2^53,
// which counts 10^15, 10^15+1 and 10^15+2 as 15 digits. Its path for larger
// coefficients checks its estimate against a power of ten, so it is exact.
func coefficientDigits(d decimal.Decimal) int64 {
	c := d.Coefficient()
	if !c.IsInt64() || c.Int64() > 1<<53 || c.Int64() < -(1<<53) {
		return int64(d.NumDigits())
	}
	v, n := c.Int64(), int64(1)
	if v < 0 {
		v = -v
	}
	for ; v >= 10; v /= 10 {
		n++
	}
	return n
}

// ParseDecimal reads a request decimal sent as a JSON number or a string
// holding one. ok is false when the field is absent or null; the caller
// decides whether that is a zero or a missing required field. The limits are
// CheckDecimal's.
func ParseDecimal(field string, raw json.RawMessage) (d decimal.Decimal, ok bool, err error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return decimal.Decimal{}, false, nil
	}
	notNumber := errs.Invalid(field, field+` must be a JSON number (5) or a string holding one ("5.5")`)
	if text[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return decimal.Decimal{}, false, notNumber
		}
		text = str
	} else if text[0] != '-' && (text[0] < '0' || text[0] > '9') {
		return decimal.Decimal{}, false, notNumber
	}
	d, err = parseDecimalText(field, text, notNumber)
	return d, err == nil, err
}

// ParseDecimalString reads a request decimal the API takes as a string only.
// Surrounding spaces are ignored.
func ParseDecimalString(field, s string) (decimal.Decimal, error) {
	return parseDecimalText(field, strings.TrimSpace(s),
		errs.Invalid(field, field+` must be a numeric string (e.g. "1000" or "3.14")`))
}

func parseDecimalText(field, text string, notNumber error) (decimal.Decimal, error) {
	if len(text) > MaxDecimalChars {
		return decimal.Decimal{}, errs.Invalid(field, fmt.Sprintf("%s is too long: it must be at most %d characters", field, MaxDecimalChars))
	}
	d, err := decimal.NewFromString(text)
	if err != nil {
		// shopspring parses any digits; on a well-formed number it fails
		// only when the exponent does not fit an int32.
		if jsonNumber.MatchString(text) {
			return decimal.Decimal{}, errs.Invalid(field, field+" cannot be read: its exponent is out of range")
		}
		return decimal.Decimal{}, notNumber
	}
	return d, nil
}

// jsonNumber is the JSON number grammar (RFC 8259, section 6).
var jsonNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
