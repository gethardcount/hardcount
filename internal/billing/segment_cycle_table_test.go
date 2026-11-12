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
func segmentDenominator(t *testing.T, ps, pe time.Time, interval domain.BillingInterval, bt domain.SubscriptionBillingTime, loc *time.Location, anchor int) int {
	t.Helper()
	plan := domain.Plan{Name: "Table", BaseAmountCents: 10000, BillingInterval: interval}
	seg := baseSegment{start: ps, end: ps.Add(24 * time.Hour), quantity: 1}
	var lines []domain.InvoiceLineItem
	var subtotal int64
	if err := emitBaseSegmentLine(seg, plan, ps, roundDays(pe.Sub(ps)), "USD", loc, bt, anchor, &lines, &subtotal); err != nil {
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

func daysIn(year int, month time.Month, loc *time.Location) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
}

// TestEmitBaseSegmentLine_DenominatorIsTheOpenedCycle walks every schedule
// period by period, as the close opens them, for anchors 1 to 31 over 36
// months in five zones, and checks that a segment is prorated over the
// period's own length: an anniversary or yearly period is the full cycle it
// was opened on, a clamped start included (Feb 28 to Mar 31 on anchor 31 is
// 31 days), and a calendar period is its month.
func TestEmitBaseSegmentLine_DenominatorIsTheOpenedCycle(t *testing.T) {
	for _, zone := range cycleTableZones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		walk := func(name string, ps time.Time, interval domain.BillingInterval, bt domain.SubscriptionBillingTime, anchor, periods int) {
			for range periods {
				pe := domain.NextBillingPeriodEnd(ps, bt, interval, loc, anchor)
				own := roundDays(pe.Sub(ps))
				if got := segmentDenominator(t, ps, pe, interval, bt, loc, anchor); got != own {
					t.Errorf("%s %s: period [%s, %s) is %d days, prorated over %d",
						zone, name, ps.In(loc).Format("2006-01-02"), pe.In(loc).Format("2006-01-02"), own, got)
				}
				ps = pe
			}
		}
		for anchor := 1; anchor <= 31; anchor++ {
			first := time.Date(2027, 1, min(anchor, 31), 0, 0, 0, 0, loc)
			walk(fmt.Sprintf("anniversary monthly anchor %d", anchor), first, domain.BillingMonthly, domain.BillingTimeAnniversary, anchor, 36)
			for month := time.January; month <= time.December; month++ {
				start := time.Date(2027, month, min(anchor, daysIn(2027, month, loc)), 0, 0, 0, 0, loc)
				walk(fmt.Sprintf("yearly anchor %d from %s", anchor, month), start, domain.BillingYearly, domain.BillingTimeAnniversary, anchor, 3)
			}
		}
		walk("calendar monthly", time.Date(2027, 1, 1, 0, 0, 0, 0, loc), domain.BillingMonthly, domain.BillingTimeCalendar, 0, 36)
	}
}
