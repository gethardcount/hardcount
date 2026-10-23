package domain

import (
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// requestLimits are the three limit sets request decimals use.
var requestLimits = map[string]DecimalLimits{
	"quantity":      QuantityLimits,
	"rate":          RateLimits,
	"provider cost": ProviderCostLimits,
}

// CheckDecimal decides both limits from the coefficient's digit count and the
// exponent. This sweeps every coefficient length up to IntDigits+Scale+2
// digits with the coefficients at the edges of each length (10^(n-1), +1, +2,
// 10^n-1), where a float digit count goes wrong, for each limit set.
//
// Mutation-verify: count digits with shopspring's NumDigits alone → the
// 16-digit coefficients go red; use > for >= in the size check → the
// 10^IntDigits cases go red.
func TestCheckDecimal_ExactAtEveryDigitCount(t *testing.T) {
	ten := big.NewInt(10)
	for name, lim := range requestLimits {
		top := lim.IntDigits // the first exponent of ten refused
		for n := int64(1); n <= lim.IntDigits+lim.Scale+2; n++ {
			lo := new(big.Int).Exp(ten, big.NewInt(n-1), nil)
			hi := new(big.Int).Sub(new(big.Int).Exp(ten, big.NewInt(n), nil), big.NewInt(1))
			for _, c := range []*big.Int{lo, new(big.Int).Add(lo, big.NewInt(1)), new(big.Int).Add(lo, big.NewInt(2)), hi} {
				for _, sign := range []int64{1, -1} {
					c := new(big.Int).Mul(c, big.NewInt(sign))
					// |c| × 10^(top+1-n) >= 10^top: refused. |c| × 10^(top-n)
					// < 10^top: taken while it has at most Scale places.
					if _, err := CheckDecimal("v", decimal.NewFromBigInt(c, int32(top+1-n)), lim); err == nil {
						t.Errorf("%s: %s e%d (>= 10^%d) accepted", name, c, top+1-n, top)
					}
					if _, err := CheckDecimal("v", decimal.NewFromBigInt(c, int32(top-n)), lim); n <= top+lim.Scale && err != nil {
						t.Errorf("%s: %s e%d (< 10^%d) refused: %v", name, c, top-n, top, err)
					}
					// c × 10^(-Scale-n+1) has Scale+n-1 written places; it fits
					// Scale only when the trailing zeros bring it down, which
					// they do for 10^(n-1).
					_, err := CheckDecimal("v", decimal.NewFromBigInt(c, int32(-lim.Scale-n+1)), lim)
					if fits := new(big.Int).Abs(c).Cmp(lo) == 0 || n == 1; fits != (err == nil) {
						t.Errorf("%s: %s e%d: fits=%v, err=%v", name, c, -lim.Scale-n+1, fits, err)
					}
				}
			}
		}
	}
}

// A zero is returned as plain 0 whatever exponent it was written with.
//
// Mutation-verify: drop the zero branch in CheckDecimal → red.
func TestCheckDecimal_ZeroIsPlainZero(t *testing.T) {
	for _, s := range []string{"0e99999999", "-0e99999999", "0e-99999999", "0.000", "-0"} {
		for name, lim := range requestLimits {
			got, err := CheckDecimal("v", decimal.RequireFromString(s), lim)
			if err != nil || !got.IsZero() || got.Exponent() != 0 {
				t.Errorf("%s %s: got %s (exponent %d), err %v; want plain 0", name, s, got, got.Exponent(), err)
			}
		}
	}
}

// The extreme exponents shopspring can hold: too large, too many places, or
// a zero.
func TestCheckDecimal_ExtremeExponents(t *testing.T) {
	for _, tc := range []struct {
		d    decimal.Decimal
		want string // "" = accepted as plain 0
	}{
		{decimal.New(1, 2147483647), "too large"},
		{decimal.New(-7, 2147483647), "too large"},
		{decimal.New(1, -2147483648), "decimal places"},
		{decimal.New(123456789, -2147483648), "decimal places"},
		{decimal.New(0, 2147483647), ""},
		{decimal.New(0, -2147483648), ""},
	} {
		for name, lim := range requestLimits {
			got, err := CheckDecimal("v", tc.d, lim)
			switch {
			case tc.want == "" && (err != nil || !got.IsZero() || got.Exponent() != 0):
				t.Errorf("%s: %se%d: got %s, err %v; want plain 0", name, tc.d.Coefficient(), tc.d.Exponent(), got, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("%s: %se%d: err %v, want %q", name, tc.d.Coefficient(), tc.d.Exponent(), err, tc.want)
			}
		}
	}
}

func TestCheckDecimal_MessagesNameTheFieldAndTheLimit(t *testing.T) {
	cases := []struct {
		d       string
		lim     DecimalLimits
		message string
	}{
		{"1e18", RateLimits, "flat_amount_cents is too large: it must be under 10^18"},
		{"1e-13", RateLimits, "flat_amount_cents has more than 12 decimal places"},
		{"100000000", ProviderCostLimits, "flat_amount_cents is too large: it must be under 10^8"},
		{"-1e26", QuantityLimits, "flat_amount_cents is too large: it must be under 10^26"},
	}
	for _, tc := range cases {
		_, err := CheckDecimal("flat_amount_cents", decimal.RequireFromString(tc.d), tc.lim)
		var de *errs.DomainError
		if !errors.As(err, &de) || de.Field != "flat_amount_cents" || de.Message != tc.message || !errors.Is(err, errs.ErrValidation) {
			t.Errorf("%s: err = %#v, want a validation error on flat_amount_cents saying %q", tc.d, err, tc.message)
		}
	}
}

// CheckRate is CheckDecimal with RateLimits plus a sign rule: 0 or more.
func TestCheckRate(t *testing.T) {
	for _, s := range []string{"0", "0.000000000001", "999999999999999999.999999999999", "1.5"} {
		got, err := CheckRate("r", decimal.RequireFromString(s))
		if err != nil || got.String() != s {
			t.Errorf("%s: got %s, err %v", s, got, err)
		}
	}
	for _, s := range []string{"-1", "-0.000000000001", "1e18", "0.0000000000001"} {
		var de *errs.DomainError
		if _, err := CheckRate("r", decimal.RequireFromString(s)); !errors.As(err, &de) || de.Field != "r" {
			t.Errorf("%s: err = %v, want a validation error on r", s, err)
		}
	}
}

// ParseDecimal reads a JSON number or a string holding one; absent and null
// are reported as not set; everything else is a validation error naming the
// field, and an exponent that does not fit is named as such.
func TestParseDecimal(t *testing.T) {
	ok := map[string]string{
		`5`: "5", `"5.5"`: "5.5", `-3`: "-3", `"-2.5"`: "-2.5", `1e3`: "1000", `"1E-3"`: "0.001",
		` 7 `: "7", `"` + "1." + strings.Repeat("0", 98) + `"`: "1",
	}
	for raw, want := range ok {
		d, set, err := ParseDecimal("q", json.RawMessage(raw))
		if err != nil || !set || d.String() != want {
			t.Errorf("%s: got %s set=%v err=%v, want %s", raw, d, set, err, want)
		}
	}
	for _, raw := range []string{``, `null`, ` null `} {
		if _, set, err := ParseDecimal("q", json.RawMessage(raw)); set || err != nil {
			t.Errorf("%q: set=%v err=%v, want not set", raw, set, err)
		}
	}
	notNumber := `q must be a JSON number (5) or a string holding one ("5.5")`
	tooLong := "q is too long: it must be at most 100 characters"
	exponent := "q cannot be read: its exponent is out of range"
	refused := map[string]string{
		`true`: notNumber, `{}`: notNumber, `[1]`: notNumber, `""`: notNumber, `"abc"`: notNumber,
		`" 5"`: notNumber, `"1e"`: notNumber, `"0x10"`: notNumber,
		`"` + "1." + strings.Repeat("0", 99) + `"`: tooLong,
		"1." + strings.Repeat("0", 99):             tooLong,
		`1e9999999999`:                             exponent, `"1e-9999999999"`: exponent, `0e9999999999`: exponent,
		`"1.5e2147483648"`: exponent,
	}
	for raw, want := range refused {
		_, set, err := ParseDecimal("q", json.RawMessage(raw))
		var de *errs.DomainError
		if set || !errors.As(err, &de) || de.Field != "q" || de.Message != want {
			t.Errorf("%s: set=%v err=%v, want %q", raw, set, err, want)
		}
	}
}

func TestParseDecimalString(t *testing.T) {
	for s, want := range map[string]string{"1000": "1000", " 3.14 ": "3.14", "1e3": "1000"} {
		if d, err := ParseDecimalString("u", s); err != nil || d.String() != want {
			t.Errorf("%q: got %s err %v, want %s", s, d, err, want)
		}
	}
	for s, want := range map[string]string{
		"":             `u must be a numeric string (e.g. "1000" or "3.14")`,
		"abc":          `u must be a numeric string (e.g. "1000" or "3.14")`,
		"1e9999999999": "u cannot be read: its exponent is out of range",
	} {
		var de *errs.DomainError
		if _, err := ParseDecimalString("u", s); !errors.As(err, &de) || de.Field != "u" || de.Message != want {
			t.Errorf("%q: err = %v, want %q", s, err, want)
		}
	}
}
