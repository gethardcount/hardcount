package domain

import (
	"math"
	"time"
)

// CyclePeriod is a billing period as the proration of its base fee sees it:
// the period's bounds, how the subscription is billed, the tenant's billing
// zone (required), and the anchor day stored on the subscription.
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
//
// Known wrong until a period carries the cycle and zone it was opened in
// (TestCycleDays_Rows pins each):
//   - A period a swap cut short whose start was a shortened month end is
//     counted from that day: 28 where its cycle (anchor 31) was 31. When the
//     cut-short period is longer than that, its own length caps it at one
//     fee, which is not the right split either.
//   - A period charged in one zone and refunded or prorated in another can be
//     divided by two numbers a day or a few apart, toward an under-refund: a
//     calendar month Feb 1 to Mar 1 opened in Asia/Kolkata is charged over
//     28 and, read in UTC, refunded over 31.
//   - A full cycle opened on anchor 29 to 31 east of the current zone is
//     prorated at a trial end: Jan 31 to Feb 28 opened in Asia/Kolkata on
//     anchor 31, read in UTC, bills 28/29.
func (p CyclePeriod) CycleDays() int {
	// The zone is the tenant's, read by the caller; a period with none is the
	// caller's bug, and In panics on it here, on every path.
	local := p.Start.In(p.Loc)
	own := wholeDays(p.End.Sub(p.Start))
	if p.Interval != BillingYearly && p.BillingTime == BillingTimeCalendar {
		return max(lastDayOfMonthIn(local.Year(), local.Month(), p.Loc), own)
	}
	if IsPeriodStartOnAnchor(p.Start, p.StoredAnchor, p.Loc) &&
		NextBillingPeriodEnd(p.Start, p.BillingTime, p.Interval, p.Loc, p.StoredAnchor).Equal(p.End) {
		return own
	}
	nominal := wholeDays(AddBillingInterval(p.Start, p.Interval, p.Loc, local.Day()).Sub(p.Start))
	return max(nominal, own)
}

// wholeDays rounds a duration to whole days. Bounds at local midnight are a
// whole number of days apart, or an hour off one across a DST change.
func wholeDays(d time.Duration) int {
	return int(math.Round(d.Hours() / 24))
}
