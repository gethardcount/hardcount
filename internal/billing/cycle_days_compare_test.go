package billing

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// gridPeriod is one period of the comparison grid, with the kind of period
// it is (which decides the readers that can see it).
type gridPeriod struct {
	kind string
	p    domain.CyclePeriod
}

// The readers' denominators as they are on main before CycleDays, frozen
// here so the comparison keeps meaning after they are replaced.
var cycleOracles = map[string]func(domain.CyclePeriod) int{
	// R1: emitBaseSegmentLine, with cycleAnchor (#1697).
	"R1": func(p domain.CyclePeriod) int {
		startDay := domain.AnchorDayFor(p.Start, p.BillingTime, p.Interval, p.Loc)
		anchor := startDay
		if startDay != 0 && p.StoredAnchor > 0 && domain.IsPeriodStartOnAnchor(p.Start, p.StoredAnchor, p.Loc) &&
			domain.NextBillingPeriodEnd(p.Start, p.BillingTime, p.Interval, p.Loc, p.StoredAnchor).Equal(p.End) {
			anchor = p.StoredAnchor
		}
		return roundDays(domain.AddBillingInterval(p.Start, p.Interval, p.Loc, anchor).Sub(p.Start))
	},
	// R2: closeBaseFeeLines in advance, with its off-anchor override.
	"R2": func(p domain.CyclePeriod) int {
		if !domain.IsPeriodStartOnAnchor(p.Start, p.StoredAnchor, p.Loc) {
			return roundDays(domain.AddBillingInterval(p.Start, p.Interval, p.Loc, 0).Sub(p.Start))
		}
		return roundDays(domain.AddBillingInterval(p.Start, p.Interval, p.Loc, p.StoredAnchor).Sub(p.Start))
	},
	// R3: onCreateBaseLines; R4: unusedBaseForPeriod; R5: the handler's
	// fullBillingCycleDays. All three advance on the stored anchor.
	"R3": storedAnchorCycle,
	"R4": storedAnchorCycle,
	"R5": storedAnchorCycle,
}

func storedAnchorCycle(p domain.CyclePeriod) int {
	return roundDays(domain.AddBillingInterval(p.Start, p.Interval, p.Loc, p.StoredAnchor).Sub(p.Start))
}

// readerSees lists the kinds of period each reader is given. The in-advance
// close (R2) bills the period it rolls, which ends where NextBillingPeriodEnd
// says in the tenant's zone now; the others read the stored period, which a
// zone change can leave as it was opened.
var readerSees = map[string][]string{
	"R1": {"plain", "calendar", "calendar-stub", "calendar-stale", "cut-short", "seam", "zone-changed", "no-anchor"},
	"R2": {"plain", "calendar", "calendar-stale", "seam", "no-anchor"},
	"R3": {"plain", "calendar", "calendar-stub"},
	"R4": {"plain", "calendar", "calendar-stub", "calendar-stale", "seam", "zone-changed", "no-anchor"},
	"R5": {"plain", "calendar", "calendar-stub", "calendar-stale", "seam", "zone-changed", "no-anchor"},
}

func daysInMonth(y int, m time.Month) int { return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day() }

// cycleGrid builds the comparison grid in each property-test zone.
func cycleGrid(t *testing.T) []gridPeriod {
	t.Helper()
	var out []gridPeriod
	add := func(kind string, p domain.CyclePeriod) {
		if p.End.After(p.Start) {
			out = append(out, gridPeriod{kind, p})
		}
	}
	for _, zone := range cycleTableZones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		at := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, loc) }
		for anchor := 1; anchor <= 31; anchor++ {
			for _, bt := range []domain.SubscriptionBillingTime{domain.BillingTimeAnniversary, domain.BillingTimeCalendar} {
				for _, iv := range []domain.BillingInterval{domain.BillingMonthly, domain.BillingYearly} {
					if bt == domain.BillingTimeCalendar && iv == domain.BillingMonthly {
						continue
					}
					months := 36
					if iv == domain.BillingYearly {
						months = 12
					}
					for k := range months {
						y, m := 2027+k/12, time.Month(k%12+1)
						start := at(y, m, min(anchor, daysInMonth(y, m)), 0)
						end := domain.NextBillingPeriodEnd(start, bt, iv, loc, anchor)
						add("plain", domain.CyclePeriod{Start: start, End: end, BillingTime: bt, Interval: iv, Loc: loc, StoredAnchor: anchor})
						// Legacy rows with no stored anchor advance by AddDate.
						legacyEnd := domain.AddBillingInterval(start, iv, loc, 0)
						add("no-anchor", domain.CyclePeriod{Start: start, End: legacyEnd, BillingTime: bt, Interval: iv, Loc: loc})
						// Cut short by an in-arrears swap on each later day, at
						// midnight and midday; the swap rewrites the anchor to its day.
						for w := start.Add(12 * time.Hour); w.Before(end) && w.Sub(start) < 62*24*time.Hour; w = w.Add(12 * time.Hour) {
							add("cut-short", domain.CyclePeriod{Start: start, End: w, BillingTime: bt, Interval: iv, Loc: loc, StoredAnchor: w.In(loc).Day()})
						}
						// After the tenant's zone changes from another one to this:
						// the period the close rolls in this zone from a boundary
						// at midnight in the other (a seam), and a period opened in
						// the other zone, read in this one.
						for _, other := range cycleTableZones {
							if other == zone {
								continue
							}
							ol, _ := time.LoadLocation(other)
							s := time.Date(y, m, min(anchor, daysInMonth(y, m)), 0, 0, 0, 0, ol)
							add("seam", domain.CyclePeriod{Start: s, End: domain.NextBillingPeriodEnd(s, bt, iv, loc, anchor), BillingTime: bt, Interval: iv, Loc: loc, StoredAnchor: anchor})
							add("zone-changed", domain.CyclePeriod{Start: s, End: domain.NextBillingPeriodEnd(s, bt, iv, ol, anchor), BillingTime: bt, Interval: iv, Loc: loc, StoredAnchor: anchor})
						}
					}
				}
			}
		}
		// Calendar monthly: full months, mid-month stubs (anchor 0), and the
		// months after a yearly-to-monthly change, which keep a stale yearly
		// anchor; also cut short by a swap.
		for k := range 36 {
			y, m := 2027+k/12, time.Month(k%12+1)
			first := at(y, m, 1, 0)
			next := domain.NextBillingPeriodEnd(first, domain.BillingTimeCalendar, domain.BillingMonthly, loc, 0)
			cal := func(kind string, s, e time.Time, anchor int) {
				add(kind, domain.CyclePeriod{Start: s, End: e, BillingTime: domain.BillingTimeCalendar, Interval: domain.BillingMonthly, Loc: loc, StoredAnchor: anchor})
			}
			cal("calendar", first, next, 0)
			for d := 2; d <= daysInMonth(y, m); d++ {
				cal("calendar-stub", at(y, m, d, 0), next, 0)
			}
			for stale := 1; stale <= 31; stale++ {
				cal("calendar-stale", first, next, stale)
				cal("calendar-stale", at(y, m, min(stale, daysInMonth(y, m)), 0), next, stale)
			}
			for w := first.Add(12 * time.Hour); w.Before(next); w = w.Add(12 * time.Hour) {
				cal("cut-short", first, w, 0)
			}
			for _, other := range cycleTableZones {
				if other == zone {
					continue
				}
				ol, _ := time.LoadLocation(other)
				s := time.Date(y, m, 1, 0, 0, 0, 0, ol)
				cal("seam", s, domain.NextBillingPeriodEnd(s, domain.BillingTimeCalendar, domain.BillingMonthly, loc, 0), 0)
				cal("zone-changed", s, domain.NextBillingPeriodEnd(s, domain.BillingTimeCalendar, domain.BillingMonthly, ol, 0), 0)
			}
		}
	}
	return out
}

// callsCycleDays lists the readers that already divide by CycleDays: they
// must give main's answer for every period they are given.
var callsCycleDays = map[string]bool{"R3": true}

// TestCycleDays_ComparedWithTheReaders compares CycleDays with each reader's
// denominator on main over every kind of period the reader is given. A reader
// that calls CycleDays must not differ; for the others, every difference must
// have a named reason, and the counts by reader and reason are logged.
func TestCycleDays_ComparedWithTheReaders(t *testing.T) {
	grid := cycleGrid(t)
	type key struct{ reader, kind string }
	seen, differ, byClass := map[key]int{}, map[key]int{}, map[key]int{}
	sample := map[key]string{}
	for _, g := range grid {
		got := g.p.CycleDays()
		if own := roundDays(g.p.End.Sub(g.p.Start)); got < own {
			t.Errorf("%s: CycleDays %d is shorter than the period's %d days: %+v", g.kind, got, own, g.p)
		}
		for reader, oracle := range cycleOracles {
			if !contains(readerSees[reader], g.kind) {
				continue
			}
			k := key{reader, g.kind}
			seen[k]++
			if want := oracle(g.p); want != got {
				differ[k]++
				class := differenceClass(g)
				byClass[key{reader, class}]++
				if callsCycleDays[reader] {
					t.Errorf("%s calls CycleDays and differs from main: %s main %d, CycleDays %d: %+v", reader, g.kind, want, got, g.p)
				} else if class == "" {
					t.Errorf("%s %s: main %d, CycleDays %d, in no named class: %+v", reader, g.kind, want, got, g.p)
				}
				if _, ok := sample[k]; !ok {
					sample[k] = fmt.Sprintf("[%s, %s) %s %s %s anchor %d: main %d, CycleDays %d",
						g.p.Start.In(g.p.Loc).Format("2006-01-02 15:04"), g.p.End.In(g.p.Loc).Format("2006-01-02 15:04"),
						g.p.Loc, g.p.BillingTime, g.p.Interval, g.p.StoredAnchor, want, got)
				}
			}
		}
	}
	keys := make([]key, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].reader != keys[j].reader {
			return keys[i].reader < keys[j].reader
		}
		return keys[i].kind < keys[j].kind
	})
	for _, k := range keys {
		t.Logf("%s %-15s %6d periods, %5d differ  %s", k.reader, k.kind, seen[k], differ[k], sample[k])
	}
	classes := make([]key, 0, len(byClass))
	for k := range byClass {
		classes = append(classes, k)
	}
	sort.Slice(classes, func(i, j int) bool {
		if classes[i].reader != classes[j].reader {
			return classes[i].reader < classes[j].reader
		}
		return classes[i].kind < classes[j].kind
	})
	for _, k := range classes {
		t.Logf("class %s %-28s %6d", k.reader, k.kind, byClass[k])
	}
}

// differenceClass names why CycleDays and main differ for a period, or ""
// when no named reason applies.
func differenceClass(g gridPeriod) string {
	p := g.p
	own := roundDays(p.End.Sub(p.Start))
	calendar := p.Interval != domain.BillingYearly && p.BillingTime == domain.BillingTimeCalendar
	switch {
	case calendar && p.StoredAnchor > 0:
		return "calendar after yearly"
	case calendar && own > daysInMonth(p.Start.In(p.Loc).Year(), p.Start.In(p.Loc).Month()):
		// Read in a new zone, a calendar period can start on the last day
		// of the month before.
		return "longer than one interval"
	case calendar:
		return ""
	case p.StoredAnchor == 0:
		return "no stored anchor"
	}
	onAnchor := domain.IsPeriodStartOnAnchor(p.Start, p.StoredAnchor, p.Loc)
	nominal := roundDays(domain.AddBillingInterval(p.Start, p.Interval, p.Loc, p.Start.In(p.Loc).Day()).Sub(p.Start))
	switch {
	case own > nominal && g.kind == "cut-short":
		return "cut short, longer than nominal"
	case own > nominal:
		return "longer than one interval"
	case g.kind == "cut-short":
		return ""
	case !onAnchor && p.Start.In(p.Loc).Day() >= 29:
		return "seam from day 29-31"
	case !onAnchor:
		return "seam"
	}
	return "seam ending off the anchor"
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
