package domain

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/gethardcount/hardcount/internal/errs"
)

// MaxLen checks that a field is not longer than max. It returns an
// errs.Invalid, so callers can show the message on the named field.
func MaxLen(field, value string, max int) error {
	if len(value) > max {
		return errs.Invalid(field, "must be at most "+strconv.Itoa(max)+" characters")
	}
	return nil
}

// validCurrencies are the common ISO 4217 currency codes ValidateCurrency
// accepts.
//
// Zero-decimal currencies (JPY, KRW, VND, CLP: currencies with no minor
// unit) are left out on purpose (ADR-100). Every Hardcount amount is in
// minor-unit cents, divided by 100 at render, and Stripe reads a zero-decimal
// amount as whole units. Accepting them would overcharge 100x at the
// PaymentIntent. They get a loud not-yet-supported error instead of a silent
// wrong charge. Restore each one only together with real zero-decimal amount
// handling.
var validCurrencies = map[string]bool{
	"USD": true, "EUR": true, "GBP": true, "CAD": true, "AUD": true,
	"CHF": true, "CNY": true, "INR": true, "BRL": true,
	"MXN": true, "SGD": true, "HKD": true, "NZD": true, "SEK": true,
	"NOK": true, "DKK": true, "ZAR": true, "TWD": true,
	"PLN": true, "CZK": true, "HUF": true, "ILS": true, "AED": true,
	"SAR": true, "THB": true, "MYR": true, "IDR": true, "PHP": true,
	"COP": true, "PEN": true, "ARS": true,
}

// zeroDecimalCurrencies get a refusal of their own, not "invalid code", so
// the operator learns the real constraint.
var zeroDecimalCurrencies = map[string]bool{
	"JPY": true, "KRW": true, "VND": true, "CLP": true,
}

// ValidateCurrency checks a currency code. Its errors are errs.Invalid or
// errs.Required, tied to the "currency" field. When the caller's form uses a
// different field name (for example "default_currency"), call errs.Invalid
// yourself with the right field and the returned message instead.
func ValidateCurrency(currency string) error {
	upper := strings.ToUpper(strings.TrimSpace(currency))
	if upper == "" {
		return errs.Required("currency")
	}
	if zeroDecimalCurrencies[upper] {
		return errs.Invalid("currency", upper+" is not supported yet: it has no minor unit, and Hardcount amounts are minor-unit cents — charging would be off by 100x. Zero-decimal currency support is tracked; use a decimal currency for now")
	}
	if !validCurrencies[upper] {
		return errs.Invalid("currency", "invalid currency code: "+upper)
	}
	return nil
}

// stripeTaxCodeFormat matches a Stripe product tax code: "txcd_" followed by
// 8 digits (for example txcd_10103001 for SaaS business use). Stripe
// maintains the full catalog. Hardcount checks only the format, to catch
// typos.
var stripeTaxCodeFormat = regexp.MustCompile(`^txcd_[0-9]{8}$`)

// ValidateStripeTaxCode checks the format of a Stripe product tax code. Empty
// is allowed, because callers supply a default when the field is optional.
func ValidateStripeTaxCode(field, code string) error {
	if code == "" {
		return nil
	}
	if !stripeTaxCodeFormat.MatchString(code) {
		return errs.Invalid(field, "must be a Stripe product tax code like 'txcd_10103001'")
	}
	return nil
}
