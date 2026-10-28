package invoice

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// dispSettings is a TenantSettingsReader that reports a fixed (live) tenant TZ.
type dispSettings struct{ tz string }

func (d dispSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{Timezone: d.tz}, nil
}

// TestInvoice_PeriodDisplay_AnchoredInBillingTZ is the invoice half of the
// ADR-074 display amendment: an invoice's inclusive period string is rendered in
// the invoice's OWN billing timezone (copied from the sub's snapshot at
// creation), not the live tenant TZ — so it can't shift a day after the tenant
// changes its timezone. Empty snapshot (ad-hoc/legacy invoice) falls back to the
// live tenant TZ, preserving prior display.
//
// The half-open period [May 2 00:00 IST, Jun 1 00:00 IST) reads "May 2 – May 31"
// in Kolkata but "May 1 – May 30" in New_York, so the assertion can't pass
// vacuously.
//
// Mutation-verify: make invoiceDisplayLoc ignore inv.BillingTimezone (always
// return s.invoiceLocation) — the snapshot case renders the New_York range and
// fails.
func TestInvoice_PeriodDisplay_AnchoredInBillingTZ(t *testing.T) {
	ctx := context.Background()
	// The tenant has SINCE moved its display TZ to New_York.
	s := &Service{settings: dispSettings{tz: "America/New_York"}}

	start := time.Date(2026, 5, 1, 18, 30, 0, 0, time.UTC) // May 2 00:00 IST
	end := time.Date(2026, 5, 31, 18, 30, 0, 0, time.UTC)  // Jun 1 00:00 IST

	cases := []struct {
		name        string
		invTZ       string
		wantLoc     string
		wantDisplay string
	}{
		{"snapshot anchors the display", "Asia/Kolkata", "Asia/Kolkata", "May 2, 2026 – May 31, 2026"},
		{"empty snapshot falls back to live tenant TZ", "", "America/New_York", "May 1, 2026 – May 30, 2026"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := domain.Invoice{
				TenantID:           "t1",
				BillingTimezone:    tc.invTZ,
				BillingPeriodStart: start,
				BillingPeriodEnd:   end,
			}
			if got := s.invoiceDisplayLoc(ctx, inv).String(); got != tc.wantLoc {
				t.Errorf("invoiceDisplayLoc: got %q, want %q", got, tc.wantLoc)
			}
			got := domain.FormatInclusivePeriod(inv.BillingPeriodStart, inv.BillingPeriodEnd, s.invoiceDisplayLoc(ctx, inv))
			if got != tc.wantDisplay {
				t.Errorf("billing_period_display: got %q, want %q", got, tc.wantDisplay)
			}
		})
	}
}
