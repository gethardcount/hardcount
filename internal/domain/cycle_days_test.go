package domain

import (
	"fmt"
	"testing"
	"time"
)

var cycleDayZones = []string{"UTC", "Asia/Kolkata", "America/New_York", "Pacific/Chatham", "Australia/Lord_Howe"}

func monthDays(y int, m time.Month) int { return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day() }

func calendarDaysBetween(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return int(time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC).Sub(time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)).Hours() / 24)
}

// TestCycleDays_FullPeriodsAreTheirOwnLength walks every anniversary monthly
// and yearly schedule for anchors 1 to 31, and calendar monthly, in five
// zones, with the boundaries counted by hand (the next month's day
// min(anchor, its days), the same month next year, the next 1st). A period
// on its anchor is a full cycle, a clamped start included: Feb 28 to Mar 31
// on anchor 31 is 31 days.
func TestCycleDays_FullPeriodsAreTheirOwnLength(t *testing.T) {
	for _, zone := range cycleDayZones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		at := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, loc) }
		check := func(name string, p CyclePeriod) {
			if want, got := calendarDaysBetween(p.Start.In(loc), p.End.In(loc)), p.CycleDays(); got != want {
				t.Errorf("%s %s [%s, %s): CycleDays %d, want %d", zone, name,
					p.Start.In(loc).Format("2006-01-02"), p.End.In(loc).Format("2006-01-02"), got, want)
			}
		}
		for anchor := 1; anchor <= 31; anchor++ {
			for k := range 36 {
				y, m := 2027+k/12, time.Month(k%12+1)
				ny, nm := y, m+1
				if nm > time.December {
					ny, nm = y+1, time.January
				}
				check(fmt.Sprintf("monthly anchor %d", anchor), CyclePeriod{
					Start: at(y, m, min(anchor, monthDays(y, m))), End: at(ny, nm, min(anchor, monthDays(ny, nm))),
					BillingTime: BillingTimeAnniversary, Interval: BillingMonthly, Loc: loc, StoredAnchor: anchor,
				})
			}
			for m := time.January; m <= time.December; m++ {
				for y := 2027; y < 2030; y++ {
					check(fmt.Sprintf("yearly anchor %d", anchor), CyclePeriod{
						Start: at(y, m, min(anchor, monthDays(y, m))), End: at(y+1, m, min(anchor, monthDays(y+1, m))),
						BillingTime: BillingTimeAnniversary, Interval: BillingYearly, Loc: loc, StoredAnchor: anchor,
					})
				}
			}
		}
		for k := range 36 {
			y, m := 2027+k/12, time.Month(k%12+1)
			end := at(y, m+1, 1)
			check("calendar", CyclePeriod{Start: at(y, m, 1), End: end, BillingTime: BillingTimeCalendar, Interval: BillingMonthly, Loc: loc})
		}
	}
}

// TestCycleDays_Rows pins the periods that are not a full cycle on their
// anchor: one row per kind the comparison with the readers found.
func TestCycleDays_Rows(t *testing.T) {
	utc := time.UTC
	ist, _ := time.LoadLocation("Asia/Kolkata")
	ny, _ := time.LoadLocation("America/New_York")
	d := func(loc *time.Location, y int, m time.Month, day, h, min int) time.Time {
		return time.Date(y, m, day, h, min, 0, 0, loc)
	}
	monthly, yearly := BillingMonthly, BillingYearly
	anniv, cal := BillingTimeAnniversary, BillingTimeCalendar
	cases := []struct {
		name string
		p    CyclePeriod
		want int
	}{
		{"calendar stub from Jan 15: January's days", CyclePeriod{d(utc, 2027, 1, 15, 0, 0), d(utc, 2027, 2, 1, 0, 0), cal, monthly, utc, 0}, 31},
		{"calendar stub from Feb 10: February's days", CyclePeriod{d(utc, 2027, 2, 10, 0, 0), d(utc, 2027, 3, 1, 0, 0), cal, monthly, utc, 0}, 28},
		{"calendar after a yearly change keeps no anchor: Apr 1 with stale anchor 15", CyclePeriod{d(utc, 2027, 4, 1, 0, 0), d(utc, 2027, 5, 1, 0, 0), cal, monthly, utc, 15}, 30},
		{"calendar stub from Jan 29 after a yearly change on anchor 29", CyclePeriod{d(utc, 2027, 1, 29, 0, 0), d(utc, 2027, 2, 1, 0, 0), cal, monthly, utc, 29}, 31},
		{"calendar period longer than its month: its own length", CyclePeriod{d(ny, 2027, 1, 1, 0, 0), d(ny, 2027, 2, 2, 0, 0), cal, monthly, ny, 0}, 32},
		{"clamped start on its anchor: Feb 28 to Mar 31 on 31", CyclePeriod{d(utc, 2027, 2, 28, 0, 0), d(utc, 2027, 3, 31, 0, 0), anniv, monthly, utc, 31}, 31},
		{"cut short, anchor rewritten to 15 at midnight Mar 15 (the old start was clamped): 28", CyclePeriod{d(ist, 2033, 2, 28, 0, 0), d(ist, 2033, 3, 15, 0, 0), anniv, monthly, ist, 15}, 28},
		{"cut short yearly from Feb 28 2031, anchor rewritten to 30: 365", CyclePeriod{d(ist, 2031, 2, 28, 0, 0), d(ist, 2031, 3, 30, 0, 0), anniv, yearly, ist, 30}, 365},
		{"cut short from Jan 15, anchor rewritten to 20", CyclePeriod{d(ist, 2033, 1, 15, 0, 0), d(ist, 2033, 1, 30, 0, 0), anniv, monthly, ist, 20}, 31},
		{"seam: a 1-day period off the anchor counts one interval from its own day", CyclePeriod{d(utc, 2027, 1, 14, 18, 30), d(utc, 2027, 1, 15, 18, 30), anniv, monthly, utc, 15}, 31},
		{"seam from Jan 31: one interval from the 31st, clamped to Feb 28", CyclePeriod{d(utc, 2027, 1, 31, 18, 30), d(utc, 2027, 2, 1, 18, 30), anniv, monthly, utc, 1}, 28},
		{"a period longer than one interval: its own length", CyclePeriod{d(utc, 2027, 1, 1, 18, 30), d(utc, 2027, 2, 2, 18, 30), anniv, monthly, utc, 2}, 32},
		{"no stored anchor (legacy row): Jan 31 to Mar 3 is its own length", CyclePeriod{d(utc, 2027, 1, 31, 0, 0), d(utc, 2027, 3, 3, 0, 0), anniv, monthly, utc, 0}, 31},
		{"no stored anchor, Jan 15 to Feb 15", CyclePeriod{d(utc, 2027, 1, 15, 0, 0), d(utc, 2027, 2, 15, 0, 0), anniv, monthly, utc, 0}, 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.CycleDays(); got != tc.want {
				t.Errorf("CycleDays = %d, want %d", got, tc.want)
			}
		})
	}
}
