package tax

import (
	"strings"
	"testing"
)

func TestNormalizeTaxID(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"27aaepm1234c1z5", "27AAEPM1234C1Z5"},
		{"  27AAEPM1234C1Z5  ", "27AAEPM1234C1Z5"},
		{"de123456789", "DE123456789"},
	}
	for _, tc := range tests {
		got := NormalizeTaxID(tc.in)
		if got != tc.want {
			t.Errorf("NormalizeTaxID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateTaxID_GSTIN(t *testing.T) {
	valid := []string{
		"27AAEPM1234C1Z5",
		"09AAACH7409R1ZZ",
	}
	for _, v := range valid {
		// Stripe-canonical kind is the primary form.
		if err := ValidateTaxID("in_gst", v); err != nil {
			t.Errorf("ValidateTaxID(in_gst, %q) unexpected error: %v", v, err)
		}
	}
	// Legacy Hardcount shorthand and the in_gstin alias must still validate.
	if err := ValidateTaxID("gstin", "27AAEPM1234C1Z5"); err != nil {
		t.Errorf("legacy gstin alias rejected: %v", err)
	}
	if err := ValidateTaxID("IN_GSTIN", "27AAEPM1234C1Z5"); err != nil {
		t.Errorf("IN_GSTIN alias rejected: %v", err)
	}

	invalid := []string{
		"ABC",              // too short
		"27AAEPM1234C1Z",   // 14 chars
		"27AAEPM1234C1Z55", // 16 chars
		"27AAEPM1234C1X5",  // no Z in reserved slot
		"AAEPM12345C1Z5",   // state code not numeric
	}
	for _, v := range invalid {
		if err := ValidateTaxID("in_gst", v); err == nil {
			t.Errorf("ValidateTaxID(in_gst, %q) expected error, got nil", v)
		}
	}

	// Lowercase input should be normalized and accepted (validator is case-insensitive).
	if err := ValidateTaxID("in_gst", "27aaepm1234c1z5"); err != nil {
		t.Errorf("lowercase GSTIN should normalize and pass: %v", err)
	}
}

func TestValidateTaxID_VAT(t *testing.T) {
	valid := []string{"DE123456789", "FRAB123456789", "GB123456789"}
	for _, v := range valid {
		if err := ValidateTaxID("eu_vat", v); err != nil {
			t.Errorf("ValidateTaxID(eu_vat, %q) unexpected error: %v", v, err)
		}
	}
	// Legacy Hardcount shorthand still validates.
	if err := ValidateTaxID("vat", "DE123456789"); err != nil {
		t.Errorf("legacy vat alias rejected: %v", err)
	}

	invalid := []string{"D1", "123456789", "DEABCDEFGHIJKLM"} // too short prefix, no prefix, too long
	for _, v := range invalid {
		if err := ValidateTaxID("eu_vat", v); err == nil {
			t.Errorf("ValidateTaxID(eu_vat, %q) expected error, got nil", v)
		}
	}
}

func TestValidateTaxID_ABN(t *testing.T) {
	if err := ValidateTaxID("au_abn", "51824753556"); err != nil {
		t.Errorf("valid ABN rejected: %v", err)
	}
	// Legacy Hardcount shorthand still validates.
	if err := ValidateTaxID("abn", "51824753556"); err != nil {
		t.Errorf("legacy abn alias rejected: %v", err)
	}
	if err := ValidateTaxID("au_abn", "51824753"); err == nil {
		t.Error("short ABN should be rejected")
	}
	if err := ValidateTaxID("au_abn", "5182475355A"); err == nil {
		t.Error("ABN with letters should be rejected")
	}
}

func TestValidateTaxID_EmptyValuePasses(t *testing.T) {
	if err := ValidateTaxID("in_gst", ""); err != nil {
		t.Errorf("empty value should pass: %v", err)
	}
	if err := ValidateTaxID("", ""); err != nil {
		t.Errorf("empty kind+value should pass: %v", err)
	}
}

func TestValidateTaxID_UnknownKindPassesThrough(t *testing.T) {
	// Jurisdictions without explicit support must not be rejected — Hardcount
	// stores the type as-is and lets Stripe / downstream tax engines apply
	// any format rules.
	if err := ValidateTaxID("br_cnpj", "12.345.678/0001-90"); err != nil {
		t.Errorf("unknown kind should pass through: %v", err)
	}
	if err := ValidateTaxID("sg_uen", "anything"); err != nil {
		t.Errorf("unknown kind should pass through: %v", err)
	}
}

func TestNormalizeTaxIDType_HardcountShorthandToCanonical(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"gstin", "in_gst"},
		{"GSTIN", "in_gst"},
		{"in_gstin", "in_gst"},
		{"in_gst", "in_gst"},
		{"vat", "eu_vat"},
		{"VAT", "eu_vat"},
		{"eu_vat", "eu_vat"},
		{"abn", "au_abn"},
		{"ABN", "au_abn"},
		{"au_abn", "au_abn"},
		// Pass-through: any other Stripe code lands as-is (lowercased).
		{"unknown_kind", "unknown_kind"},
		{"za_vat", "za_vat"},
		{"  us_ein  ", "us_ein"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := NormalizeTaxIDType(tc.in); got != tc.want {
			t.Errorf("NormalizeTaxIDType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateTaxID_AcceptsBothFormats(t *testing.T) {
	// Same value must validate identically under canonical Stripe codes
	// and legacy Hardcount shorthand — required so old data continues to work
	// without a backfill.
	pairs := []struct {
		canonical, legacy, value string
	}{
		{"in_gst", "gstin", "27AAEPM1234C1Z5"},
		{"eu_vat", "vat", "DE123456789"},
		{"au_abn", "abn", "51824753556"},
	}
	for _, p := range pairs {
		errCanonical := ValidateTaxID(p.canonical, p.value)
		errLegacy := ValidateTaxID(p.legacy, p.value)
		if (errCanonical == nil) != (errLegacy == nil) {
			t.Errorf("ValidateTaxID disagrees for %q: canonical(%q)=%v, legacy(%q)=%v",
				p.value, p.canonical, errCanonical, p.legacy, errLegacy)
		}
	}
}

// Each refusal shows the operator an example; the example must pass.
func TestValidateTaxID_RefusalExamplePasses(t *testing.T) {
	for _, kind := range []string{"in_gst", "eu_vat", "au_abn"} {
		err := ValidateTaxID(kind, "!")
		if err == nil {
			t.Fatalf("ValidateTaxID(%s, \"!\") expected error, got nil", kind)
		}
		_, example, ok := strings.Cut(err.Error(), ", like ")
		if !ok {
			t.Fatalf("%s refusal %q gives no example", kind, err)
		}
		if err := ValidateTaxID(kind, example); err != nil {
			t.Errorf("%s refusal's example %q is refused: %v", kind, example, err)
		}
	}
}
