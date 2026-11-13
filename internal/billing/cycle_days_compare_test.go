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
// zone change can leave as it was opened. The on-create lines (R3) bill a new
// subscription's first period, the trial's first paid period at trial end
// (written at create or ExtendTrial, so possibly in another zone), the
// in-advance swap's period from the swap's instant (BillOnPlanSwapTx), and
// the trial-end preview (previewTrialEnd).
var readerSees = map[string][]string{
	"R1": {"plain", "calendar", "calendar-stub", "calendar-stale", "cut-short", "seam", "zone-changed", "no-anchor", "swap"},
	"R2": {"plain", "calendar", "calendar-stale", "seam", "no-anchor"},
	"R3": {"plain", "calendar", "calendar-stub", "zone-changed", "swap"},
	"R4": {"plain", "calendar", "calendar-stub", "calendar-stale", "seam", "zone-changed", "no-anchor", "swap"},
	"R5": {"plain", "calendar", "calendar-stub", "calendar-stale", "seam", "zone-changed", "no-anchor", "swap"},
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
						// The in-advance swap opens a period at the swap's instant,
						// anchored on its day.
						sw := time.Date(y, m, min(anchor, daysInMonth(y, m)), 13, 17, 0, 0, loc)
						swAnchor := domain.AnchorDayFor(sw, bt, iv, loc)
						add("swap", domain.CyclePeriod{Start: sw, End: domain.NextBillingPeriodEnd(sw, bt, iv, loc, swAnchor), BillingTime: bt, Interval: iv, Loc: loc, StoredAnchor: swAnchor})
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
				cal("swap", time.Date(y, m, d, 13, 17, 0, 0, loc), next, 0)
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

// changed is, for each reader and named reason, how many periods of the grid
// it now divides by a different number of days than on main: the behaviour
// this change makes, counted. Any other count fails the test.
var changed = map[[2]string]int{
	{"R1", "longer than one interval"}:       18424,
	{"R1", "cut short, longer than nominal"}: 142,
	{"R1", "no stored anchor"}:               100,
	{"R2", "longer than one interval"}:       17800,
	{"R2", "seam from day 29-31"}:            200,
	{"R2", "calendar after yearly"}:          200,
	{"R3", "seam"}:                           16740,
	{"R3", "seam from day 29-31"}:            1230,
	{"R3", "longer than one interval"}:       580,
	{"R4", "seam"}:                           16780,
	{"R4", "calendar after yearly"}:          5600,
	{"R4", "seam from day 29-31"}:            1790,
	{"R4", "longer than one interval"}:       580,
	{"R5", "seam"}:                           16780,
	{"R5", "calendar after yearly"}:          5600,
	{"R5", "seam from day 29-31"}:            1790,
	{"R5", "longer than one interval"}:       580,
}

// TestCycleDays_ComparedWithTheReaders compares CycleDays, which every reader
// now divides by, with each reader's denominator on main over every kind of
// period the reader is given. Every difference must have a named reason,
// CycleDays must be the value the design gives for it, and the count for each
// reader and reason is the one in changed.
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
			want := oracle(g.p)
			if want == got {
				continue
			}
			differ[k]++
			class, designed := differenceClass(g)
			byClass[key{reader, class}]++
			switch {
			case class == "":
				t.Errorf("%s %s: main %d, CycleDays %d, in no named class: %+v", reader, g.kind, want, got, g.p)
			case got != designed:
				t.Errorf("%s %s %q: CycleDays %d, the design gives %d: %+v", reader, g.kind, class, got, designed, g.p)
			}
			if _, ok := sample[k]; !ok {
				sample[k] = fmt.Sprintf("[%s, %s) %s %s %s anchor %d: main %d, CycleDays %d",
					g.p.Start.In(g.p.Loc).Format("2006-01-02 15:04"), g.p.End.In(g.p.Loc).Format("2006-01-02 15:04"),
					g.p.Loc, g.p.BillingTime, g.p.Interval, g.p.StoredAnchor, want, got)
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
		t.Logf("class %s %-30s %6d", k.reader, k.kind, byClass[k])
	}
	for k, n := range byClass {
		if k.kind != "" && changed[[2]string{k.reader, k.kind}] != n {
			t.Errorf("%s %q: %d periods differ from main, want %d", k.reader, k.kind, n, changed[[2]string{k.reader, k.kind}])
		}
	}
	for k, want := range changed {
		if byClass[key{k[0], k[1]}] == 0 {
			t.Errorf("%s %q: no period differs from main, want %d", k[0], k[1], want)
		}
	}
}

// differenceClass names why CycleDays and main can differ for a period, and
// the number of days the design gives it for that reason, computed from the
// period's shape: the period's own length, one interval from its start on
// its own day (clamped), or the days in its start's month. It returns ""
// when no named reason applies.
func differenceClass(g gridPeriod) (string, int) {
	p := g.p
	local := p.Start.In(p.Loc)
	own := roundDays(p.End.Sub(p.Start))
	monthDays := daysInMonth(local.Year(), local.Month())
	calendar := p.Interval != domain.BillingYearly && p.BillingTime == domain.BillingTimeCalendar
	switch {
	case calendar && own > monthDays:
		// Read in a new zone, a calendar period can start on the last day of
		// the month before.
		return "longer than one interval", own
	case calendar && p.StoredAnchor > 0:
		return "calendar after yearly", monthDays
	case calendar:
		return "", 0
	case p.StoredAnchor == 0:
		// A legacy period advanced by AddDate is at least as long as one
		// interval from its own day.
		return "no stored anchor", own
	}
	nominal := roundDays(domain.AddBillingInterval(p.Start, p.Interval, p.Loc, local.Day()).Sub(p.Start))
	onAnchor := domain.IsPeriodStartOnAnchor(p.Start, p.StoredAnchor, p.Loc)
	switch {
	case own > nominal && g.kind == "cut-short":
		return "cut short, longer than nominal", own
	case own > nominal:
		return "longer than one interval", own
	case g.kind == "cut-short", onAnchor:
		return "", 0
	case local.Day() >= 29:
		return "seam from day 29-31", nominal
	default:
		return "seam", nominal
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
