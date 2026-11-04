// Package timeline holds the two ordering rules that every activity or
// timeline surface must keep:
//
//  1. Sort axis equals display axis, at full precision. A surface orders by
//     the same instant it renders: the simulated effect time on a row
//     pinned to a test clock, and wall time otherwise. It compares the
//     time.Time, never a serialized copy. RFC3339 strings truncate to
//     seconds, which can put same-second pairs in the wrong order (#518,
//     #521).
//  2. Exact ties order by cause. On a frozen test clock, a whole close
//     cascade (finalize, dunning start, retries, escalate, write-off,
//     resolve) rightly shares one instant. Insertion order there groups
//     rows by source, which can show "Marked uncollectible" above the
//     escalation that caused it. Ties need an explicit causal rule, chosen
//     by the surface: a rank, an id in creation order, or a full-precision
//     recorded-at.
//
// SortStable is shaped on purpose so that a new surface cannot adopt it
// without answering both questions: what instant do I display, and how do
// exact ties order?
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

// SortStable orders events ascending by their displayed instant `at`, at
// full precision. It consults `tieLess` only when two instants are exactly
// equal. The sort is stable, so rows equal under both keep insertion order.
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

// Order sorts activity rows ascending, by the one rule every activity
// surface shares (ADR-128, from ADR-104's 2026-07-29 amendment):
//
//  1. at, the instant the row shows on the entity's calendar, at full
//     precision.
//  2. Inside a group of rows at exactly the same instant, the real order
//     (recorded), but only when every row in the group has a recorded
//     stamp. A group with a bare row is legacy data. Placing bare rows
//     among stamped ones would make up a sequence, so that group keeps
//     rule 3.
//  3. tieLess, the cause-before-effect rank.
//  4. Insertion order.
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
