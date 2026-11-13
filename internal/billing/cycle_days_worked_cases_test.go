package billing

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// One worked case for each reason a reader now divides by a different number
// of days than before CycleDays (TestCycleDays_ComparedWithTheReaders counts
// them). Each period is one the close writes. The fee is 10000c. The comment
// on each case gives the amount before. "No stored anchor" is the legacy row
// of TestEmitBaseSegmentLine_DenominatorFromPeriodAnchor.

// The in-arrears close (R1).
func TestCycleDays_WorkedCases_InArrearsClose(t *testing.T) {
	plan := domain.Plan{Name: "Worked", BaseAmountCents: 10000, BillingInterval: domain.BillingMonthly}
	day := 24 * time.Hour
	cases := []struct {
		name       string
		start, end time.Time
		anchor     int
		split      time.Time // the change between the two segments
		want       [2]int64
	}{
		// Longer than one interval: the tenant moved from Asia/Kolkata to
		// UTC, and the close rolled the period from IST's Jan 2 boundary in
		// UTC, on anchor 2: 32 days. Before: 16/31 + 16/31 = 5161 + 5161 =
		// 10322, more than the fee.
		{"a 32-day seam billed in two 16-day segments", utcAt(2027, 1, 1, 18, 30), utcAt(2027, 2, 2, 18, 30), 2,
			utcAt(2027, 1, 1, 18, 30).Add(16 * day), [2]int64{5000, 5000}},
		// Cut short, longer than nominal: an anchor-31 period from Feb 28 cut
		// short by a swap at Mar 28 12:00 (anchor rewritten to 28) is 28.5
		// days, rounded to 29. Before: 15/28 + 14/28 = 5357 + 5000 = 10357.
		{"a cut-short period longer than a month from its own day", utcAt(2027, 2, 28, 0, 0), utcAt(2027, 3, 28, 12, 0), 28,
			utcAt(2027, 3, 15, 0, 0), [2]int64{5172, 4828}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []domain.InvoiceLineItem
			var subtotal int64
			for _, seg := range []baseSegment{{start: tc.start, end: tc.split, quantity: 1}, {start: tc.split, end: tc.end, quantity: 1}} {
				if err := emitBaseSegmentLine(seg, plan, domain.CyclePeriod{Start: tc.start, End: tc.end, BillingTime: domain.BillingTimeAnniversary, Loc: time.UTC, StoredAnchor: tc.anchor}, "USD", &lines, &subtotal); err != nil {
					t.Fatalf("emitBaseSegmentLine: %v", err)
				}
			}
			if len(lines) != 2 || lines[0].AmountCents != tc.want[0] || lines[1].AmountCents != tc.want[1] {
				t.Fatalf("lines = %v, want amounts %v", amounts(lines), tc.want)
			}
			if subtotal != 10000 {
				t.Errorf("subtotal = %d, want the fee, 10000", subtotal)
			}
		})
	}
}

// The unused base fee a cancel or a swap credits (R4).
func TestCycleDays_WorkedCases_UnusedBase(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{pricing: &mockPricing{plans: map[string]domain.Plan{
		"pln": {ID: "pln", BaseAmountCents: 10000, BaseBillTiming: domain.BillInAdvance, BillingInterval: domain.BillingMonthly},
	}}}
	cases := []struct {
		name       string
		bt         domain.SubscriptionBillingTime
		start, end time.Time
		anchor     int
		at         time.Time
		want       int64
	}{
		// Seam from day 29-31: the close rolled a 1-day period in UTC from
		// IST's Feb 1 boundary on anchor 1. Before, its cycle was the seam
		// itself (1 day), so a cancel at its start refunded the whole fee,
		// 10000. Now one month from Jan 31, clamped: 1/28.
		{"a cancel at the start of a 1-day seam", domain.BillingTimeAnniversary,
			utcAt(2027, 1, 31, 18, 30), utcAt(2027, 2, 1, 18, 30), 1, utcAt(2027, 1, 31, 18, 30), 357},
		// Seam: a period opened in IST on anchor 15, read after the tenant
		// moved to UTC, starts on Jan 14 there. Before: 32 days (to Feb 15
		// in UTC), 10/32 = 3125. Now its own 31 days: 10/31.
		{"a cancel 10 days before the end of a period opened in another zone", domain.BillingTimeAnniversary,
			time.Date(2027, 1, 15, 0, 0, 0, 0, ist), time.Date(2027, 2, 15, 0, 0, 0, 0, ist), 15,
			time.Date(2027, 2, 5, 0, 0, 0, 0, ist), 3226},
		// Calendar after a yearly change: the anchor of the old yearly plan
		// (15) stays on the row. Before: Apr 1 to May 15, 44 days, 15/44 =
		// 3409. Now April's 30 days: 15/30.
		{"a cancel on Apr 16 of a calendar month with a stale yearly anchor", domain.BillingTimeCalendar,
			utcAt(2027, 4, 1, 0, 0), utcAt(2027, 5, 1, 0, 0), 15, utcAt(2027, 4, 16, 0, 0), 5000},
		// The period's end decides: Feb 28 to Mar 31 on anchor 31 is a full
		// 31-day cycle, so 15 days left are 15/31. With any other end the
		// period would read as cut short and count 28 days from the 28th
		// (5357).
		{"a cancel on Mar 16 of Feb 28 to Mar 31 on anchor 31", domain.BillingTimeAnniversary,
			utcAt(2027, 2, 28, 0, 0), utcAt(2027, 3, 31, 0, 0), 31, utcAt(2027, 3, 16, 0, 0), 4839},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := domain.Subscription{TenantID: "t1", BillingTime: tc.bt, BillingAnchorDay: tc.anchor,
				Items: []domain.SubscriptionItem{{PlanID: "pln", Quantity: 1}}}
			got, err := e.unusedBaseForPeriod(context.Background(), sub, tc.start, tc.end, tc.at, time.UTC)
			if err != nil {
				t.Fatalf("unusedBaseForPeriod: %v", err)
			}
			if got != tc.want {
				t.Errorf("unused base = %d, want %d", got, tc.want)
			}
		})
	}
}

func utcAt(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, time.UTC)
}

func amounts(lines []domain.InvoiceLineItem) []int64 {
	out := make([]int64, len(lines))
	for i, li := range lines {
		out[i] = li.AmountCents
	}
	return out
}

// The in-advance close (R2), for the period it rolls.
func TestCycleDays_WorkedCases_InAdvanceClose(t *testing.T) {
	plans := map[string]domain.Plan{
		"pln": {ID: "pln", Name: "Worked", BaseAmountCents: 10000, BaseBillTiming: domain.BillInAdvance, BillingInterval: domain.BillingMonthly},
	}
	// Seam from day 29-31: the tenant moved from Asia/Kolkata to UTC, and the
	// period that closes at IST's Feb 1 boundary (Jan 31 18:30 UTC) rolls a
	// 1-day period on anchor 1. Before: one month from Jan 31 by AddDate, 31
	// days, 1/31 = 323. Now one month from the 31st, clamped: 1/28.
	sub := domain.Subscription{TenantID: "t1", BillingTime: domain.BillingTimeAnniversary, BillingAnchorDay: 1,
		Items: []domain.SubscriptionItem{{ID: "it1", PlanID: "pln", Quantity: 1}}}
	lines, err := closeBaseFeeLines(sub, nil, plans, utcAt(2027, 1, 1, 0, 0), utcAt(2027, 1, 31, 18, 30), "USD", time.UTC, false, false)
	if err != nil {
		t.Fatalf("closeBaseFeeLines: %v", err)
	}
	if len(lines) != 1 || lines[0].line.AmountCents != 357 {
		var got []int64
		for _, l := range lines {
			got = append(got, l.line.AmountCents)
		}
		t.Fatalf("in-advance base lines = %v, want one of 357 (1/28)", got)
	}
}
