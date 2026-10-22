package tenant

import (
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// TestValidateSettings_CompanyNameControlChars covers the medium-severity
// audit finding: company_name flows into the From: display name of every
// outbound email, so a CR/LF in it was an email-header-injection vector.
// validateSettings now rejects control characters at write time.
func TestValidateSettings_CompanyNameControlChars(t *testing.T) {
	t.Run("clean name accepted", func(t *testing.T) {
		ts := &domain.TenantSettings{CompanyName: "Acme Inc"}
		if err := validateSettings(ts); err != nil {
			t.Fatalf("clean company_name rejected: %v", err)
		}
	})

	for name, val := range map[string]string{
		"CRLF":         "Acme\r\nBcc: victim@evil.com",
		"bare LF":      "Acme\nEvil",
		"bare CR":      "Acme\rEvil",
		"DEL":          "Acme\x7f",
		"null byte":    "Acme\x00",
		"vertical tab": "Acme\x0b",
	} {
		t.Run("rejected/"+name, func(t *testing.T) {
			ts := &domain.TenantSettings{CompanyName: val}
			err := validateSettings(ts)
			if err == nil {
				t.Fatalf("expected rejection for %q", val)
			}
			if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "company_name" {
				t.Errorf("expected company_name validation error, got %v", err)
			}
		})
	}
}

// The tax-rate refusal is shown under the Tax rate field, so it states the
// range in plain words (it used to end in "ADR-042/043.").
func TestValidateSettings_TaxRateReason(t *testing.T) {
	for _, rate := range []float64{-1, 100.0001, 118} {
		err := validateSettings(&domain.TenantSettings{TaxRate: rate})
		if errs.Field(err) != "tax_rate" || err.Error() != "must be from 0 to 100 (for 18.5% enter 18.5)" {
			t.Errorf("rate %v: got %v (field %q)", rate, err, errs.Field(err))
		}
	}
	for _, rate := range []float64{0, 18.5, 100} {
		if err := validateSettings(&domain.TenantSettings{TaxRate: rate}); errs.Field(err) == "tax_rate" {
			t.Errorf("rate %v refused: %v", rate, err)
		}
	}
}

// credit_note_prefix starts every credit note number, the same way
// invoice_prefix starts every invoice number. It was stored with a default of
// CN but no API field could set it (C2), so every tenant's notes read
// CN-000001. It gets the invoice prefix's check: the value goes into PDF file
// names and email subjects.
func TestValidateSettings_CreditNotePrefix(t *testing.T) {
	if got := DefaultSettings("t").CreditNotePrefix; got != "CN" {
		t.Errorf("default credit_note_prefix = %q, want CN", got)
	}

	ts := &domain.TenantSettings{CreditNotePrefix: ""}
	if err := validateSettings(ts); err != nil {
		t.Fatalf("empty prefix refused: %v", err)
	}
	if ts.CreditNotePrefix != "CN" {
		t.Errorf("empty prefix stored as %q, want the default CN", ts.CreditNotePrefix)
	}

	ts = &domain.TenantSettings{CreditNotePrefix: "  ACME-CN "}
	if err := validateSettings(ts); err != nil {
		t.Fatalf("valid prefix refused: %v", err)
	}
	if ts.CreditNotePrefix != "ACME-CN" {
		t.Errorf("prefix stored as %q, want it trimmed to ACME-CN", ts.CreditNotePrefix)
	}

	for _, bad := range []string{"ACME CN", "ACME/CN", "CN_1", "ABCDEFGHIJKLMNOPQRSTU"} {
		err := validateSettings(&domain.TenantSettings{CreditNotePrefix: bad})
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "credit_note_prefix" {
			t.Errorf("prefix %q: got %v (field %q), want a credit_note_prefix validation error", bad, err, errs.Field(err))
		}
	}
}
