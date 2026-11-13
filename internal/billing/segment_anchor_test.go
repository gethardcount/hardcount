package billing

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestEmitBaseSegmentLine_DenominatorFromPeriodAnchor: the proration
// denominator is one full cycle from the period's start, counted on the
// anchor CycleDays reconstructs (domain.CyclePeriod).
//
// A period not cut short ends where its start and the stored anchor say, and
// is counted on the stored anchor: Feb 28 to Mar 31 on anchor 31 is 31 days.
// A cross-interval swap cuts the period short and rewrites the stored anchor,
// so a cut-short period is counted on its start's own day, as before: a
// calendar period from Jan 1 IST with the anchor rewritten to 15 divides by
// 31, not 46, and an anniversary one from Jan 15 rewritten to 20 by 31.
//
// Two rows pin what a cut-short period gets: a yearly period from Feb 28
// 2031 on anchor 28, cut short on Mar 30 with the anchor rewritten to 30,
// divides by 365, as before this change (the clamp would have said 366); and
// the known wrong case, a monthly period from Feb 28 2033 whose old anchor
// was 31, cut short at midnight on Mar 15 with the anchor rewritten to 15,
// divides by 28 where its cycle was 31. Its end is where Feb 28 and anchor
// 15 would end a period, so only the start's test keeps it off anchor 15. A
// legacy period with no stored anchor (Jan 31 to Mar 3 by AddDate) divides by
// its own 31 days, not the 28 from the start's own day.
func TestEmitBaseSegmentLine_DenominatorFromPeriodAnchor(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	inIST := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, ist) }
	monthly := domain.Plan{Name: "Seg Plan", BaseAmountCents: 10000, BillingInterval: domain.BillingMonthly}
	yearly := domain.Plan{Name: "Seg Plan", BaseAmountCents: 10000, BillingInterval: domain.BillingYearly}

	cases := []struct {
		name        string
		plan        domain.Plan
		start, end  time.Time
		billingTime domain.SubscriptionBillingTime
		stored      int
		wantDays    int
		wantAmount  int64 // 10000 x 15/wantDays
	}{
		{"calendar period, cut short, anchor rewritten to 15", monthly, inIST(2033, 1, 1), inIST(2033, 1, 16), domain.BillingTimeCalendar, 15, 31, 4839},
		{"anniversary from Jan 15, cut short, anchor rewritten to 20", monthly, inIST(2033, 1, 15), inIST(2033, 1, 30), domain.BillingTimeAnniversary, 20, 31, 4839},
		{"anniversary from Jan 15 on its anchor", monthly, inIST(2033, 1, 15), inIST(2033, 2, 15), domain.BillingTimeAnniversary, 15, 31, 4839},
		{"anniversary from Feb 28 on anchor 31", monthly, inIST(2033, 2, 28), inIST(2033, 3, 31), domain.BillingTimeAnniversary, 31, 31, 4839},
		{"yearly from Feb 28 2031, cut short Mar 30, anchor rewritten to 30", yearly, inIST(2031, 2, 28), inIST(2031, 3, 30), domain.BillingTimeAnniversary, 30, 365, 411},
		{"known wrong: monthly from Feb 28 2033, cut short at midnight Mar 15, anchor rewritten to 15", monthly, inIST(2033, 2, 28), inIST(2033, 3, 15), domain.BillingTimeAnniversary, 15, 28, 5357},
		{"no stored anchor: a legacy period from Jan 31 is its own length", monthly, inIST(2033, 1, 31), inIST(2033, 3, 3), domain.BillingTimeAnniversary, 0, 31, 4839},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seg := baseSegment{start: tc.start, end: tc.start.Add(15 * 24 * time.Hour), quantity: 1}
			var lines []domain.InvoiceLineItem
			var subtotal int64
			if err := emitBaseSegmentLine(seg, tc.plan, domain.CyclePeriod{Start: tc.start, End: tc.end, BillingTime: tc.billingTime, Loc: ist, StoredAnchor: tc.stored}, "USD", &lines, &subtotal); err != nil {
				t.Fatalf("emitBaseSegmentLine: %v", err)
			}
			if len(lines) != 1 {
				t.Fatalf("lines: got %d, want 1", len(lines))
			}
			if want := fmt.Sprintf("prorated 15/%d days", tc.wantDays); !strings.Contains(lines[0].Description, want) {
				t.Errorf("description %q: want %q", lines[0].Description, want)
			}
			if lines[0].AmountCents != tc.wantAmount {
				t.Errorf("amount: got %d, want %d", lines[0].AmountCents, tc.wantAmount)
			}
		})
	}
}
