package tax

import (
	"errors"
	"regexp"
	"strings"
)

// Tax-ID kind canonical codes. We use Stripe's documented codes as primary
// (see https://stripe.com/docs/api/customer_tax_ids/object) so operators see
// the same identifiers in Stripe webhooks, the Stripe Dashboard, and Hardcount.
// Hardcount shorthand (gstin/vat/abn) is still accepted on input — see
// NormalizeTaxIDType — so existing data continues to work without a backfill.
const (
	TaxIDKindINGST = "in_gst" // India GSTIN — Stripe canonical
	TaxIDKindEUVAT = "eu_vat" // EU VAT — Stripe canonical
	TaxIDKindAUABN = "au_abn" // Australia ABN — Stripe canonical
)

// gstinPattern matches the Indian GSTIN format:
//
//	2 digits (state code) + 10 chars PAN (AAAAA9999A) + 1 digit entity code
//	+ 'Z' (reserved) + 1 alphanumeric check character
//
// Example: 27AAEPM1234C1Z5. Full checksum validation requires additional work
// and is not performed here — format check catches the vast majority of typos
// and intentional invalid inputs, which is the audit requirement for
// phase-lite. Upgrade to checksum validation when we have a paying Indian
// customer under a live GST registration.
var gstinPattern = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z]{1}[1-9A-Z]{1}Z[0-9A-Z]{1}$`)

// euVATPattern accepts the common EU VAT shape: 2-letter country prefix
// followed by up to 12 alphanumeric characters. Country-specific length and
// check-digit rules vary; network-based validation via VIES is a phase-2
// concern and not performed here.
var euVATPattern = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{2,12}$`)

// australianABNPattern: 11 digits.
var australianABNPattern = regexp.MustCompile(`^[0-9]{11}$`)

// NormalizeTaxIDType maps any accepted alias for a tax-ID kind to its
// canonical Stripe code so storage stays consistent. Unknown kinds pass
// through untouched (lowercased + trimmed) — Hardcount doesn't reject
// jurisdictions it hasn't added explicit format support for. Empty input
// returns empty.
func NormalizeTaxIDType(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "gstin", "in_gstin", TaxIDKindINGST:
		return TaxIDKindINGST
	case "vat", TaxIDKindEUVAT:
		return TaxIDKindEUVAT
	case "abn", TaxIDKindAUABN:
		return TaxIDKindAUABN
	}
	return k
}

// ValidateTaxID checks that a tax ID conforms to the format for its declared
// kind. Returns nil for unknown kinds (pass-through) so we don't reject tax
// IDs from jurisdictions we haven't added explicit support for. An empty
// value is always valid — callers should check presence separately if the
// field is required.
//
// Both the canonical Stripe code (in_gst, eu_vat, au_abn) and the legacy
// Hardcount shorthand (gstin, vat, abn) are accepted as input so previously
// stored data still validates after the canonical-rename. New writes
// normalize to the Stripe code via NormalizeTaxIDType before persistence.
//
// Kind matching is case-insensitive on the key; the value itself is
// normalized (uppercased, whitespace trimmed) before format validation.
func ValidateTaxID(kind, value string) error {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case TaxIDKindINGST, "gstin", "in_gstin":
		if !gstinPattern.MatchString(value) {
			return errors.New("must be a 15-character GSTIN, like 27AAEPM1234C1Z5")
		}
	case TaxIDKindEUVAT, "vat":
		if !euVATPattern.MatchString(value) {
			return errors.New("must be a 2-letter country code, then 2 to 12 letters or digits, like DE123456789")
		}
	case TaxIDKindAUABN, "abn":
		if !australianABNPattern.MatchString(value) {
			return errors.New("must be 11 digits, like 51824753556")
		}
	}
	return nil
}

// NormalizeTaxID returns the canonical storage form: whitespace-trimmed and
// uppercased for schemes that are case-insensitive by spec. Safe to call
// before validation; returns the input unchanged when it's empty.
func NormalizeTaxID(value string) string {
	v := strings.TrimSpace(value)
	return strings.ToUpper(v)
}
