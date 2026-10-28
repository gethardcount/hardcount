package billing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// usageFloors.start: the window [s, e) opens at the latest of s, the trial end
// and another subscription's coverage end strictly inside the window.
func TestUsageFloors_Start(t *testing.T) {
	d := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return d.Add(time.Duration(n) * time.Hour) }
	ptr := func(t time.Time) *time.Time { return &t }

	cases := []struct {
		name   string
		floors usageFloors
		s, e   time.Time
		want   time.Time
	}{
		{"no floors", usageFloors{}, d, h(24), d},
		{"trial end inside", usageFloors{trialEnd: ptr(h(18))}, d, h(24), h(18)},
		{"trial end before the window", usageFloors{trialEnd: ptr(h(-6))}, d, h(24), d},
		{"trial end after the window: empty", usageFloors{trialEnd: ptr(h(30))}, d, h(24), h(30)},
		{"coverage end inside", usageFloors{coverageEnds: map[string][]time.Time{"m": {h(10)}}}, d, h(24), h(10)},
		{"latest coverage end wins, any order", usageFloors{coverageEnds: map[string][]time.Time{"m": {h(12), h(10)}}}, d, h(24), h(12)},
		{"another meter's end does not count", usageFloors{coverageEnds: map[string][]time.Time{"other": {h(10)}}}, d, h(24), d},
		// A later subscription's coverage ends after an earlier one's window
		// does: it never moves the earlier window, at the window end
		// included (the predecessor keeps its whole window).
		{"coverage end at the window end", usageFloors{coverageEnds: map[string][]time.Time{"m": {h(10)}}}, d, h(10), d},
		{"coverage end after the window", usageFloors{coverageEnds: map[string][]time.Time{"m": {h(12)}}}, d, h(10), d},
		{"coverage end at the window start", usageFloors{coverageEnds: map[string][]time.Time{"m": {d}}}, d, h(24), d},
		{"coverage end before the trial end", usageFloors{trialEnd: ptr(h(18)), coverageEnds: map[string][]time.Time{"m": {h(10)}}}, d, h(24), h(18)},
		{"coverage end after the trial end", usageFloors{trialEnd: ptr(h(8)), coverageEnds: map[string][]time.Time{"m": {h(10)}}}, d, h(24), h(10)},
	}
	for _, c := range cases {
		if got := c.floors.start("m", c.s, c.e); !got.Equal(c.want) {
			t.Errorf("%s: start = %v, want %v", c.name, got, c.want)
		}
	}
}

// meterUsageIntervals applies the floors to the clamped AND the full map: the
// full map carries deferred max/last buckets (ADR-066 §4), and trial usage is
// nobody's in those either. The watermark clamps only the clamped map.
func TestMeterUsageIntervals_FloorsBothMaps_WatermarkOnlyClamped(t *testing.T) {
	d := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	pe := d.AddDate(0, 1, 0)
	trialEnd := d.Add(18 * time.Hour)
	wm := d.Add(21 * time.Hour)
	sub := domain.Subscription{Items: []domain.SubscriptionItem{{ID: "it", PlanID: "p"}}}
	plans := map[string][]string{"p": {"m"}}
	m := planMeter{planID: "p", meterID: "m"}
	segs := map[string][]baseSegment{"it": {{start: d, end: pe, planID: "p"}}}

	clamped, full := meterUsageIntervals(planStretches(sub, segs, d, pe), plans, &wm, usageFloors{trialEnd: &trialEnd})
	if got := clamped[m]; len(got) != 1 || !got[0].start.Equal(wm) || !got[0].end.Equal(pe) {
		t.Errorf("clamped = %v, want [[watermark, period end)]", got)
	}
	if got := full[m]; len(got) != 1 || !got[0].start.Equal(trialEnd) || !got[0].end.Equal(pe) {
		t.Errorf("full = %v, want [[trial end, period end)]: the deferred max bucket must not reach trial usage", got)
	}

	// A window wholly before the floor drops out of both maps.
	early := map[string][]baseSegment{"it": {{start: d, end: d.Add(10 * time.Hour), planID: "p"}}}
	clamped, full = meterUsageIntervals(planStretches(sub, early, d, pe), plans, &wm, usageFloors{trialEnd: &trialEnd})
	if len(clamped[m]) != 0 || len(full[m]) != 0 {
		t.Errorf("a segment wholly in the trial: clamped %v, full %v; want both empty", clamped[m], full[m])
	}
}

// loadUsageFloors fails loud: a window built without its floor bills usage
// another subscription billed, so a read error stops the closer.
func TestLoadUsageFloors_ReadErrorFailsLoud(t *testing.T) {
	boom := errors.New("db down")
	subs := &mockSubs{coverageEndsErr: boom}
	e := &Engine{}
	e.SetIntervalReader(subs)
	d := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	if _, err := e.loadUsageFloors(context.Background(), domain.Subscription{ID: "s", CustomerID: "c"}, d, d.AddDate(0, 1, 0)); !errors.Is(err, boom) {
		t.Fatalf("loadUsageFloors error = %v, want the read error", err)
	}

	subs.coverageEndsErr = nil
	subs.coverageEnds = []domain.MeterCoverageEnd{{MeterID: "m", EndsAt: d.Add(10 * time.Hour)}, {MeterID: "m", EndsAt: d.Add(12 * time.Hour)}}
	f, err := e.loadUsageFloors(context.Background(), domain.Subscription{ID: "s", CustomerID: "c"}, d, d.AddDate(0, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.start("m", d, d.AddDate(0, 1, 0)); !got.Equal(d.Add(12 * time.Hour)) {
		t.Errorf("start = %v, want the latest coverage end", got)
	}
}

// subtractUsageIntervals takes the taken time out of each interval: a hole
// in the middle splits it, an edge trims it, and a cover removes it.
func TestSubtractUsageIntervals(t *testing.T) {
	h := func(n int) time.Time { return time.Date(2026, 4, 10, n, 0, 0, 0, time.UTC) }
	iv := func(a, b int) usageInterval { return usageInterval{h(a), h(b)} }
	cases := []struct {
		name       string
		ivs, taken []usageInterval
		want       []usageInterval
	}{
		{"nothing taken", []usageInterval{iv(0, 10)}, nil, []usageInterval{iv(0, 10)}},
		{"hole in the middle", []usageInterval{iv(0, 10)}, []usageInterval{iv(2, 4)}, []usageInterval{iv(0, 2), iv(4, 10)}},
		{"front and back", []usageInterval{iv(0, 10)}, []usageInterval{iv(0, 2), iv(8, 12)}, []usageInterval{iv(2, 8)}},
		{"covered", []usageInterval{iv(2, 4)}, []usageInterval{iv(0, 10)}, nil},
		{"touching only", []usageInterval{iv(2, 4)}, []usageInterval{iv(0, 2), iv(4, 6)}, []usageInterval{iv(2, 4)}},
		{"two intervals, two holes", []usageInterval{iv(0, 4), iv(6, 10)}, []usageInterval{iv(1, 2), iv(3, 7)}, []usageInterval{iv(0, 1), iv(2, 3), iv(7, 10)}},
	}
	for _, c := range cases {
		got := subtractUsageIntervals(c.ivs, c.taken)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
