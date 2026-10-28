package billing

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/tax"
)

// TestTaxRetryBackoff verifies the schedule produces strictly
// increasing delays per attempt and stays within ±10% of the
// declared base for each step.
func TestTaxRetryBackoff(t *testing.T) {
	expectedBase := []time.Duration{
		5 * time.Minute,
		15 * time.Minute,
		1 * time.Hour,
		4 * time.Hour,
		12 * time.Hour,
		24 * time.Hour,
		48 * time.Hour,
		96 * time.Hour,
	}
	for i, base := range expectedBase {
		got := taxRetryBackoff(i)
		min := time.Duration(float64(base) * 0.9)
		max := time.Duration(float64(base) * 1.1)
		if got < min || got > max {
			t.Errorf("attempt %d: got %v, want within ±10%% of %v", i, got, base)
		}
	}
}

// TestTaxRetryBackoff_OutOfRangeClamps confirms callers that pass
// an attempts value past the schedule's last index get the final
// (4d ± jitter) bucket — the schedule "saturates" rather than
// returning zero.
func TestTaxRetryBackoff_OutOfRangeClamps(t *testing.T) {
	got := taxRetryBackoff(99)
	min := time.Duration(float64(96*time.Hour) * 0.9)
	if got < min {
		t.Errorf("out-of-range attempt: got %v, want >= %v (clamped to last bucket)", got, min)
	}
}

// TestNextTaxRetry_Outcomes covers the four decision branches:
// success, retryable-under-cap, non-retryable, retryable-at-cap.
// TestTaxRetryPolicy_DomainAndTaxPackageAgree pins domain's plain-string
// retry policy (which the attention banner asserts to operators) to the
// tax package's typed error codes — domain cannot import tax, so the two
// lists are textual twins; this is the gate that keeps them twins.
func TestTaxRetryPolicy_DomainAndTaxPackageAgree(t *testing.T) {
	want := map[string]bool{
		string(tax.ErrCodeProviderOutage):        true,
		string(tax.ErrCodeUnknown):               true,
		string(tax.ErrCodeProviderNotConfigured): true,
	}
	got := domain.TaxRetryableErrorCodes()
	if len(got) != len(want) {
		t.Fatalf("retryable codes: got %v, want the %d codes %v", got, len(want), want)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("domain lists %q as retryable; tax package has no matching retryable const", c)
		}
	}
	if maxTaxRetryAttempts != domain.MaxTaxRetryAttempts {
		t.Errorf("retry cap drift: billing=%d domain=%d", maxTaxRetryAttempts, domain.MaxTaxRetryAttempts)
	}
}

func TestNextTaxRetry_Outcomes(t *testing.T) {
	cases := []struct {
		name     string
		status   domain.InvoiceTaxStatus
		errCode  string
		attempts int
		wantNil  bool
	}{
		{"success clears retry", domain.InvoiceTaxOK, "", 3, true},
		{"retryable-under-cap schedules", domain.InvoiceTaxPending, string(tax.ErrCodeProviderOutage), 0, false},
		{"unknown is also retryable", domain.InvoiceTaxPending, string(tax.ErrCodeUnknown), 2, false},
		{"not-configured is retryable (pre-flight, self-heals post-connect)", domain.InvoiceTaxPending, string(tax.ErrCodeProviderNotConfigured), 1, false},
		{"non-retryable code skips", domain.InvoiceTaxPending, string(tax.ErrCodeProviderAuth), 0, true},
		{"customer-data is non-retryable", domain.InvoiceTaxPending, string(tax.ErrCodeCustomerDataInvalid), 0, true},
		{"final attempt clears", domain.InvoiceTaxPending, string(tax.ErrCodeProviderOutage), maxTaxRetryAttempts - 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextTaxRetry(context.Background(), tc.status, tc.errCode, tc.attempts)
			if tc.wantNil && got != nil {
				t.Errorf("expected nil, got %v", *got)
			}
			if !tc.wantNil && got == nil {
				t.Errorf("expected non-nil retry timestamp")
			}
		})
	}
}
