package billing

import (
	"fmt"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// The zones the date property tests use.
var cycleTableZones = []string{"UTC", "Asia/Kolkata", "America/New_York", "Pacific/Chatham", "Australia/Lord_Howe"}

var proratedOver = regexp.MustCompile(`prorated 1/(\d+) days`)

// segmentDenominator bills a one-day segment at the start of [ps, pe) and
// returns the days the line was prorated over.
func segmentDenominator(t *testing.T, ps, pe time.Time, interval domain.BillingInterval, bt domain.SubscriptionBillingTime, loc *time.Location, anchor, periodDays int) int {
	t.Helper()
	plan := domain.Plan{Name: "Table", BaseAmountCents: 10000, BillingInterval: interval}
	seg := baseSegment{start: ps, end: ps.Add(24 * time.Hour), quantity: 1}
	var lines []domain.InvoiceLineItem
	var subtotal int64
	if err := emitBaseSegmentLine(seg, plan, ps, pe, periodDays, "USD", loc, bt, anchor, &lines, &subtotal); err != nil {
		t.Fatalf("emitBaseSegmentLine: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("lines: %d, want 1", len(lines))
	}
	m := proratedOver.FindStringSubmatch(lines[0].Description)
	if m == nil {
		t.Fatalf("description %q: not prorated", lines[0].Description)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// daysIn is the number of days in month m of year y.
func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// civilDays counts the calendar days from (y1, m1, d1) to (y2, m2, d2).
func civilDays(y1 int, m1 time.Month, d1, y2 int, m2 time.Month, d2 int) int {
	return int(time.Date(y2, m2, d2, 0, 0, 0, 0, time.UTC).Sub(time.Date(y1, m1, d1, 0, 0, 0, 0, time.UTC)).Hours() / 24)
}

// TestEmitBaseSegmentLine_DenominatorIsTheOpenedCycle walks every schedule
// period by period for anchors 1 to 31 over 36 months in five zones, with
// the boundaries counted by hand (the next month's day min(anchor, its
// days), the same month next year, or the next 1st), and checks that a
// segment is prorated over the period's own length in calendar days: a
// clamped start included, Feb 28 to Mar 31 on anchor 31 is 31 days.
func TestEmitBaseSegmentLine_DenominatorIsTheOpenedCycle(t *testing.T) {
	type date struct {
		y int
		m time.Month
		d int
	}
	for _, zone := range cycleTableZones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		at := func(d date) time.Time { return time.Date(d.y, d.m, d.d, 0, 0, 0, 0, loc) }
		walk := func(name string, from date, next func(date) date, interval domain.BillingInterval, bt domain.SubscriptionBillingTime, anchor, periods int) {
			for range periods {
				to := next(from)
				want := civilDays(from.y, from.m, from.d, to.y, to.m, to.d)
				ps, pe := at(from), at(to)
				if got := segmentDenominator(t, ps, pe, interval, bt, loc, anchor, want); got != want {
					t.Errorf("%s %s: period [%04d-%02d-%02d, %04d-%02d-%02d) is %d days, prorated over %d",
						zone, name, from.y, from.m, from.d, to.y, to.m, to.d, want, got)
				}
				from = to
			}
		}
		for anchor := 1; anchor <= 31; anchor++ {
			nextMonth := func(d date) date {
				y, m := d.y, d.m+1
				if m > time.December {
					y, m = y+1, time.January
				}
				return date{y, m, min(anchor, daysIn(y, m))}
			}
			nextYear := func(d date) date { return date{d.y + 1, d.m, min(anchor, daysIn(d.y+1, d.m))} }
			walk(fmt.Sprintf("anniversary monthly anchor %d", anchor), date{2027, time.January, min(anchor, 31)}, nextMonth,
				domain.BillingMonthly, domain.BillingTimeAnniversary, anchor, 36)
			for m := time.January; m <= time.December; m++ {
				walk(fmt.Sprintf("yearly anchor %d from %s", anchor, m), date{2027, m, min(anchor, daysIn(2027, m))}, nextYear,
					domain.BillingYearly, domain.BillingTimeAnniversary, anchor, 3)
			}
		}
		nextFirst := func(d date) date {
			if d.m == time.December {
				return date{d.y + 1, time.January, 1}
			}
			return date{d.y, d.m + 1, 1}
		}
		walk("calendar monthly", date{2027, time.January, 1}, nextFirst, domain.BillingMonthly, domain.BillingTimeCalendar, 0, 36)
	}
}
