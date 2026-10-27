package billing

import (
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestEmitBaseSegmentLine_DenominatorFromPeriodAnchor pins the FLOW B21
// find (2026-07-21): the proration denominator must come from the period
// being billed, not the subscription's live billing_anchor_day. A
// cross-interval swap rewrites the sub's anchor before the truncated old
// period closes; pre-fix the call sites passed that mutated anchor and a
// calendar-monthly period [Jan 1 → Jan 15 IST] closed at "prorated 15/46
// days" (denominator stretched to the new day-15 anchor's Feb 15) instead
// of 15/31 — under-billing 3,261¢ vs the correct 4,839¢ on a $100 base.
// Mutation seam: reintroduce an anchorDay parameter fed by a sub-level
// value ≠ the period's own anchor and both sub-tests fail.
func TestEmitBaseSegmentLine_DenominatorFromPeriodAnchor(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	plan := domain.Plan{Name: "Seg Plan", BaseAmountCents: 10000, BillingInterval: domain.BillingMonthly}

	cases := []struct {
		name        string
		periodStart time.Time
		billingTime domain.SubscriptionBillingTime
	}{
		// Calendar sub: period opens Jan 1 IST; AnchorDayFor returns 0 and
		// the denominator is the plain month Jan 1 → Feb 1 = 31 days.
		{"calendar period", time.Date(2032, 12, 31, 18, 30, 0, 0, time.UTC), domain.BillingTimeCalendar},
		// Anniversary sub anchored day 15: Jan 15 → Feb 15 = 31 days.
		{"anniversary period", time.Date(2033, 1, 14, 18, 30, 0, 0, time.UTC), domain.BillingTimeAnniversary},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seg := baseSegment{start: tc.periodStart, end: tc.periodStart.Add(15 * 24 * time.Hour), quantity: 1}
			var lines []domain.InvoiceLineItem
			var subtotal int64
			if err := emitBaseSegmentLine(seg, plan, tc.periodStart, 15, "USD", ist, tc.billingTime, &lines, &subtotal); err != nil {
				t.Fatalf("emitBaseSegmentLine: %v", err)
			}
			if len(lines) != 1 {
				t.Fatalf("lines: got %d, want 1", len(lines))
			}
			if !strings.Contains(lines[0].Description, "prorated 15/31 days") {
				t.Errorf("description %q: want denominator 31 (the period's own cycle), independent of any sub-level anchor", lines[0].Description)
			}
			if lines[0].AmountCents != 4839 {
				t.Errorf("amount: got %d, want 4839 (10000 × 15/31)", lines[0].AmountCents)
			}
		})
	}
}
