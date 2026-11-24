// Package timeline holds the two ordering rules that every activity or
// timeline surface must keep:
//
//  1. Sort axis equals display axis, at full precision. A surface orders by
//     the same instant it renders, and compares the time.Time, never a
//     serialized copy. RFC3339 strings truncate to seconds, which can put
//     same-second pairs in the wrong order (#518, #521).
//  2. Exact ties order by cause. Work that binds one instant (a close:
//     finalize, dunning start, escalate, write-off, resolve) dates every row
//     it writes at that instant. Insertion order there groups rows by
//     source, which can show "Marked uncollectible" above the escalation
//     that caused it. Ties need an explicit causal rule, chosen by the
//     surface: a rank, or an id in creation order.
//
// SortStable is shaped on purpose so that a new surface cannot adopt it
// without answering both questions: what instant do I display, and how do
// exact ties order?
package timeline

import (
	"sort"
	"time"
)

// SortStable orders events ascending by their displayed instant `at`, at
// full precision. It consults `tieLess` only when two instants are exactly
// equal. The sort is stable, so rows equal under both `at` and `tieLess` keep
// insertion order.
//
// `at` must return the instant the surface displays for the row. If the two
// differ, the rendered order contradicts the rendered timestamps, which is
// the class of bug this package exists to prevent.
func SortStable[E any](events []E, at func(E) time.Time, tieLess func(a, b E) bool) {
	sort.SliceStable(events, func(i, j int) bool {
		ai, aj := at(events[i]), at(events[j])
		if !ai.Equal(aj) {
			return ai.Before(aj)
		}
		return tieLess(events[i], events[j])
	})
}
