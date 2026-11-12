package billing

import (
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestEmitBaseSegmentLine_DenominatorFromPeriodAnchor: the proration
// denominator is the cycle the period was opened on.
//
// A cross-interval swap rewrites the subscription's stored anchor before the
// truncated old period closes. That period's start is not on the new anchor,
// so its cycle is counted from its own day: a calendar period [Jan 1, Feb 1)
// IST with a stored anchor of 15 divides by 31, not 46 (Jan 1 to Feb 15), and
// an anniversary period from Jan 15 with a stored anchor of 20 divides by 31.
//
// A start clamped to a shorter month's end lies on the higher stored anchor:
// Feb 28 on anchor 31 opened the cycle Feb 28 to Mar 31, 31 days, not the 28
// its own day would give.
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
		stored      int
	}{
		{"calendar period, anchor rewritten to 15", time.Date(2032, 12, 31, 18, 30, 0, 0, time.UTC), domain.BillingTimeCalendar, 15},
		{"anniversary period from Jan 15, anchor rewritten to 20", time.Date(2033, 1, 14, 18, 30, 0, 0, time.UTC), domain.BillingTimeAnniversary, 20},
		{"anniversary period from Jan 15 on its anchor", time.Date(2033, 1, 14, 18, 30, 0, 0, time.UTC), domain.BillingTimeAnniversary, 15},
		{"anniversary period from Feb 28 on anchor 31", time.Date(2033, 2, 27, 18, 30, 0, 0, time.UTC), domain.BillingTimeAnniversary, 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seg := baseSegment{start: tc.periodStart, end: tc.periodStart.Add(15 * 24 * time.Hour), quantity: 1}
			var lines []domain.InvoiceLineItem
			var subtotal int64
			if err := emitBaseSegmentLine(seg, plan, tc.periodStart, 15, "USD", ist, tc.billingTime, tc.stored, &lines, &subtotal); err != nil {
				t.Fatalf("emitBaseSegmentLine: %v", err)
			}
			if len(lines) != 1 {
				t.Fatalf("lines: got %d, want 1", len(lines))
			}
			if !strings.Contains(lines[0].Description, "prorated 15/31 days") {
				t.Errorf("description %q: want denominator 31, the cycle the period opened on", lines[0].Description)
			}
			if lines[0].AmountCents != 4839 {
				t.Errorf("amount: got %d, want 4839 (10000 × 15/31)", lines[0].AmountCents)
			}
		})
	}
}
