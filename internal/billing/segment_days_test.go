package billing

import (
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestEmitBaseSegmentLine_DaysAddUpToThePeriod (#1274): each segment's days
// are the change instants rounded to whole days from the period start, so
// the segments of a period add up to the period whatever the change times.
// Rounding each segment on its own billed 31 days of 30 here (11 + 11 + 9),
// and 29 when the change times round down.
//
// Mutation seam: round each segment's own length again and this fails.
func TestEmitBaseSegmentLine_DaysAddUpToThePeriod(t *testing.T) {
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	plan := domain.Plan{Name: "Seats", BaseAmountCents: 3000, BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears}
	day := 24 * time.Hour

	cases := []struct {
		name     string
		changes  []time.Duration // offsets of the change instants from the period start
		wantDays []int
	}{
		// The issue's example: 10.6 and 21.2 days in.
		{"changes that round up", []time.Duration{day*10 + 14*time.Hour + 24*time.Minute, day*21 + 4*time.Hour + 48*time.Minute}, []int{11, 10, 9}},
		// 10.4 and 20.8 days in: each segment alone rounds to 10, 10 and 9.
		{"changes that round down", []time.Duration{day*10 + 9*time.Hour + 36*time.Minute, day*20 + 19*time.Hour + 12*time.Minute}, []int{10, 11, 9}},
		// A segment under half a day that crosses a day's rounding point
		// keeps its day, and the one that does not loses it.
		{"a short segment", []time.Duration{day*10 + 10*time.Hour, day*10 + 14*time.Hour}, []int{10, 1, 19}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bounds := []time.Time{periodStart}
			for _, c := range tc.changes {
				bounds = append(bounds, periodStart.Add(c))
			}
			bounds = append(bounds, periodEnd)

			var lines []domain.InvoiceLineItem
			var subtotal, wantSubtotal int64
			days := 0
			for i := range len(bounds) - 1 {
				qty := int64(i + 1)
				seg := baseSegment{start: bounds[i], end: bounds[i+1], quantity: qty}
				if err := emitBaseSegmentLine(seg, plan, periodStart, 30, "USD", time.UTC, domain.BillingTimeCalendar, &lines, &subtotal); err != nil {
					t.Fatalf("segment %d: %v", i, err)
				}
				days += tc.wantDays[i]
				wantSubtotal += 3000 * qty * int64(tc.wantDays[i]) / 30
			}
			if days != 30 {
				t.Fatalf("test case days add up to %d, want 30", days)
			}
			gotDays := 0
			for i, li := range lines {
				d := 30 // a full-period line carries no "prorated" note
				if m := proratedDays.FindStringSubmatch(li.Description); m != nil {
					d, _ = strconv.Atoi(m[1])
				}
				gotDays += d
				if d != tc.wantDays[i] {
					t.Errorf("segment %d: %d days (%q), want %d", i, d, li.Description, tc.wantDays[i])
				}
			}
			if gotDays != 30 || subtotal != wantSubtotal {
				t.Errorf("segments bill %d days for %dc, want 30 days for %dc", gotDays, subtotal, wantSubtotal)
			}
		})
	}
}

// proratedDays reads the days from a base line's "prorated N/M days" note.
var proratedDays = regexp.MustCompile(`prorated (\d+)/\d+ days`)
