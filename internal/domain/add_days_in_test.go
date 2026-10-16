package domain

import (
	"testing"
	"time"
)

// TestAddDaysIn_CrossesDSTOnCivilCalendar: Net 30 from a cycle that closes at
// 2026-11-01 00:00 EDT must be due 2026-12-01 00:00 EST. A 30×24h add lands at
// 2026-11-30 23:00 EST, which prints "November 30" (one day early) because the
// span crosses the end of DST on Nov 1.
func TestAddDaysIn_CrossesDSTOnCivilCalendar(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	cases := []struct {
		name string
		at   time.Time
		n    int
		loc  *time.Location
		want time.Time
	}{
		{"fall back: Nov 1 EDT + 30 → Dec 1 EST midnight", time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), 30, ny, time.Date(2026, 12, 1, 5, 0, 0, 0, time.UTC)},
		{"spring forward: Mar 1 EST + 14 → Mar 15 EDT midnight", time.Date(2026, 3, 1, 5, 0, 0, 0, time.UTC), 14, ny, time.Date(2026, 3, 15, 4, 0, 0, 0, time.UTC)},
		{"no DST change: same as a 24h add", time.Date(2026, 6, 1, 4, 0, 0, 0, time.UTC), 30, ny, time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC)},
		{"zero days (due on receipt) is the same instant", time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), 0, ny, time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC)},
		{"nil loc falls back to UTC", time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), 30, nil, time.Date(2026, 12, 1, 4, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AddDaysIn(tc.at, tc.n, tc.loc)
			if !got.Equal(tc.want) {
				t.Fatalf("AddDaysIn(%s, %d) = %s, want %s", tc.at.Format(time.RFC3339), tc.n, got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			if got.Location() != time.UTC {
				t.Errorf("result location = %s, want UTC", got.Location())
			}
		})
	}
	// The civil date in the tenant zone is what the PDF and dashboard print.
	due := AddDaysIn(time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), 30, ny)
	if got := due.In(ny).Format("January 2, 2006"); got != "December 1, 2026" {
		t.Errorf("printed due date = %q, want %q", got, "December 1, 2026")
	}
}
