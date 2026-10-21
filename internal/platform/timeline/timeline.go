// Package timeline carries the two ordering invariants every activity /
// timeline surface must satisfy, mechanized after the same two bugs
// shipped on three separate surfaces each (2026-07-19 audit):
//
//  1. SORT AXIS == DISPLAY AXIS, AT FULL PRECISION. A surface orders by
//     the same instant it renders — the simulated effect time on
//     clock-pinned rows, wall time otherwise — and compares the
//     time.Time, never a serialized copy (RFC3339 strings truncate to
//     seconds, which inverted same-second pairs on the subscription
//     timeline, #518, and the invoice timeline, #521).
//  2. EXACT TIES ORDER CAUSALLY. On a frozen test clock an entire close
//     cascade (finalize → dunning start → retries → escalate →
//     write-off → resolve) legitimately shares ONE instant; insertion
//     order there is source-major, which rendered "Marked
//     uncollectible" above the escalation that caused it. Ties need an
//     explicit causal rule — a rank, a creation-ordered id, a
//     full-precision recorded-at — chosen by the surface.
//
// SortStable is deliberately shaped so a new surface cannot adopt it
// without answering both questions: what instant do I display, and how
// do exact ties order?
//
// Order is the one ordering rule for activity surfaces (ADR-128): the
// entity-calendar instant, then the real order inside a same-instant group
// whose every row has a recorded stamp, then the cause-before-effect rank,
// then insertion order.
package timeline

import (
	"sort"
	"time"
)

// SortStable orders events ascending by their displayed instant `at`
// (full precision), consulting `tieLess` ONLY on exact equality. The
// sort is stable, so rows equal under both axes keep insertion order.
//
// `at` must return the instant the surface DISPLAYS for the row — if
// these diverge, the rendered order contradicts the rendered
// timestamps, which is the class this package exists to prevent.
func SortStable[E any](events []E, at func(E) time.Time, tieLess func(a, b E) bool) {
	sort.SliceStable(events, func(i, j int) bool {
		ai, aj := at(events[i]), at(events[j])
		if !ai.Equal(aj) {
			return ai.Before(aj)
		}
		return tieLess(events[i], events[j])
	})
}

// Order sorts activity rows ascending, by the one rule every activity
// surface shares (ADR-128, from ADR-104's 2026-07-29 amendment):
//
//  1. at, the instant the row shows on the entity's calendar, at full
//     precision;
//  2. inside a group of rows at exactly the same instant, the real order
//     (recorded), but only when EVERY row in the group has a recorded
//     stamp. A group with a bare row is legacy data, and placing bare rows
//     among stamped ones would make up a sequence, so it keeps rule 3;
//  3. tieLess, the cause-before-effect rank;
//  4. insertion order.
//
// recorded returns the zero time for a row with no real-world stamp. Rows
// with equal recorded stamps (written in one transaction) keep rule 3's
// order.
func Order[E any](rows []E, at, recorded func(E) time.Time, tieLess func(a, b E) bool) {
	SortStable(rows, at, tieLess)
	for lo := 0; lo < len(rows); {
		hi := lo + 1
		for hi < len(rows) && at(rows[hi]).Equal(at(rows[lo])) {
			hi++
		}
		complete := true
		for i := lo; i < hi; i++ {
			if recorded(rows[i]).IsZero() {
				complete = false
				break
			}
		}
		if complete && hi-lo > 1 {
			group := rows[lo:hi]
			sort.SliceStable(group, func(a, b int) bool {
				return recorded(group[a]).Before(recorded(group[b]))
			})
		}
		lo = hi
	}
}
