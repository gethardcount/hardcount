package domain

import (
	"math"
	"time"
)

// CyclePeriod is a billing period as the proration of its base fee sees it:
// the period's bounds, how the subscription is billed, and the anchor day
// stored on the subscription.
type CyclePeriod struct {
	Start, End   time.Time
	BillingTime  SubscriptionBillingTime
	Interval     BillingInterval
	Loc          *time.Location
	StoredAnchor int
}

// CycleDays is the number of days a share of the period's base fee is
// prorated over: one full billing cycle, never shorter than the period.
//
//   - Calendar monthly: the days in the start's month, or the period's own
//     length if longer.
//   - A period on the stored anchor is a full cycle: its own length. Feb 28
//     to Mar 31 on anchor 31 is 31 days.
//   - Any other period (an anniversary seam after a time-zone change, or a
//     period a plan swap cut short): one interval from the start on its own
//     day, clamped to the month's end, or the period's own length if longer.
//
// A period is on the anchor when its start lies on the stored anchor (clamped
// to the month's end) and it ends where that start and anchor say. The end
// test sends a period cut short by a swap, which rewrote the stored anchor,
// to the nominal branch. A stored anchor of 0 (a row from before the column)
// counts as on the anchor and advances by plain month arithmetic, so such a
// period is its own length.
func (p CyclePeriod) CycleDays() int {
	loc := p.Loc
	if loc == nil {
		loc = time.UTC
	}
	own := wholeDays(p.End.Sub(p.Start))
	if p.Interval != BillingYearly && p.BillingTime == BillingTimeCalendar {
		local := p.Start.In(loc)
		return max(lastDayOfMonthIn(local.Year(), local.Month(), loc), own)
	}
	if IsPeriodStartOnAnchor(p.Start, p.StoredAnchor, loc) &&
		NextBillingPeriodEnd(p.Start, p.BillingTime, p.Interval, loc, p.StoredAnchor).Equal(p.End) {
		return own
	}
	nominal := wholeDays(AddBillingInterval(p.Start, p.Interval, loc, p.Start.In(loc).Day()).Sub(p.Start))
	return max(nominal, own)
}

// wholeDays rounds a duration to whole days. Bounds at local midnight are a
// whole number of days apart, or an hour off one across a DST change.
func wholeDays(d time.Duration) int {
	return int(math.Round(d.Hours() / 24))
}
