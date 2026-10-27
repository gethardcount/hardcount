package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// ADR-101 Phase 4: the single segment source.
//
// Every base-fee and usage segment the engine bills derives from
// billing_intervals — policy-applied item lifetimes recorded at write
// time. The read side is a dumb interval×window intersection with
// nothing to interpret. The legacy fact-log interpretation, the shadow
// comparator, and the VELOX_BILLING_INTERVALS_READER modes were removed
// at Phase 4 (2026-07-28) after a clean soak: zero unexplained
// divergences across the cutover sweep (140/140 subs), the two-mode CI
// corpus on every PR since, and every dev cycle close in between.
// There is no legacy fallback — a missing interval row for a live item
// is a WRITER bug and fails the sub's billing loudly.

// IntervalSnapshotter is the engine's consumer-defined seam for the
// interval read. *subscription.PostgresStore satisfies it directly.
type IntervalSnapshotter interface {
	// ListItemIntervals returns the sub's full interval history,
	// ordered (starts_at, then zero-width/closed rows before the row
	// that reopened at the same instant). Deliberately UNWINDOWED: the
	// reader clips in Go, and the missing-interval invariant must
	// distinguish "no rows overlap this window" (legitimate zero — a
	// lifetime entirely outside the window) from "no rows AT ALL" (a
	// writer bug).
	ListItemIntervals(ctx context.Context, tenantID, subscriptionID string) ([]domain.ItemInterval, error)
	// ListCustomerMeterCoverageEnds returns, per meter, the instants in the
	// open window (from, to) at which the customer's OTHER subscriptions
	// (every one but excludeSubID) stopped covering it: a billing interval
	// of non-zero width on a plan carrying the meter ended, or a
	// never-activated trial whose live items carry it was canceled. The
	// usage window's floor (usageFloors) is built from them.
	ListCustomerMeterCoverageEnds(ctx context.Context, tenantID, customerID, excludeSubID string, from, to time.Time) ([]domain.MeterCoverageEnd, error)
}

// SetIntervalReader wires the interval read seam. The snapshotter is a
// required collaborator (MustValidate refuses to boot without it);
// panicking here on nil keeps narrow tests honest — an engine that
// bills needs interval data, there is no other reader.
func (e *Engine) SetIntervalReader(snap IntervalSnapshotter) {
	if snap == nil {
		panic("SetIntervalReader: nil snapshotter — billing_intervals is the only segment source (ADR-101 Phase 4)")
	}
	e.intervalSnap = snap
}

// windowSegments resolves the per-item base segments for one billing
// window [windowStart, windowEnd] from billing_intervals. The returned
// map covers live items AND items that left the sub during the window
// (their sealed intervals still intersect it); callers iterate it for
// the removed-item pass.
func (e *Engine) windowSegments(ctx context.Context, sub domain.Subscription, windowStart, windowEnd time.Time) (map[string][]baseSegment, error) {
	rowsByItem, err := e.intervalRows(ctx, sub)
	if err != nil {
		return nil, err
	}
	return intervalSegmentsByItem(rowsByItem, windowStart, windowEnd), nil
}

// intervalRows reads sub's billing intervals, by item.
func (e *Engine) intervalRows(ctx context.Context, sub domain.Subscription) (map[string][]domain.ItemInterval, error) {
	intervals, err := e.intervalSnap.ListItemIntervals(ctx, sub.TenantID, sub.ID)
	if err != nil {
		return nil, fmt.Errorf("billing-intervals read: %w", err)
	}
	rowsByItem := map[string][]domain.ItemInterval{}
	for _, iv := range intervals {
		rowsByItem[iv.SubscriptionItemID] = append(rowsByItem[iv.SubscriptionItemID], iv)
	}

	// The loud missing-interval invariant. A live item with NO interval
	// rows at all would silently bill zero forever — that is a writer
	// bug, and the sub's billing fails instead. An item whose rows
	// simply don't overlap this window is a legitimate zero (its
	// lifetime starts after the window).
	for _, it := range sub.Items {
		if len(rowsByItem[it.ID]) == 0 {
			return nil, fmt.Errorf("billing-intervals: active item %s on sub %s has no interval rows (writer bug — refusing to bill it as zero)", it.ID, sub.ID)
		}
	}
	return rowsByItem, nil
}

// estimateSegments is, for an estimate over [from, to] (customer usage over
// a chosen window, a preview of one), sub's segments over the window and the
// pins it is priced at: those of sub's current period (pinsOf), from one
// read of its billing intervals. An estimate is priced at today's prices,
// not at each past period's own (ADR-130 §10); a plan not on sub in the
// current period is read at that period's start. A subscription with no
// current period is pinned as if its period started at from.
func (e *Engine) estimateSegments(ctx context.Context, sub domain.Subscription, from, to time.Time) (map[string][]baseSegment, periodPins, error) {
	rowsByItem, err := e.intervalRows(ctx, sub)
	if err != nil {
		return nil, periodPins{}, err
	}
	segs := intervalSegmentsByItem(rowsByItem, from, to)
	if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
		return segs, pinsOf(sub, planStretches(sub, segs, from, to), from), nil
	}
	start, end := *sub.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodEnd
	current := planStretches(sub, intervalSegmentsByItem(rowsByItem, start, end), start, end)
	return segs, pinsOf(sub, current, start), nil
}

// intervalSegmentsByItem derives segments from billing_intervals rows:
// clip each row to the window, drop empty slices, merge adjacent
// equal-(plan, qty) slices. No policy here — the day-grade decisions
// were made at write time; this is the "dumb intersection" the ADR
// promises. Each segment carries its item's creation.
func intervalSegmentsByItem(rowsByItem map[string][]domain.ItemInterval, windowStart, windowEnd time.Time) map[string][]baseSegment {
	out := make(map[string][]baseSegment, len(rowsByItem))
	for itemID, rows := range rowsByItem {
		var segs []baseSegment
		for _, iv := range rows {
			start := iv.StartsAt
			if start.Before(windowStart) {
				start = windowStart
			}
			end := windowEnd
			if iv.EndsAt != nil && iv.EndsAt.Before(windowEnd) {
				end = *iv.EndsAt
			}
			if !end.After(start) {
				continue
			}
			segs = append(segs, baseSegment{start: start, end: end, planID: iv.PlanID, quantity: iv.Quantity, itemCreatedAt: iv.ItemCreatedAt})
		}
		if merged := mergeEqualAdjacent(segs); len(merged) > 0 {
			out[itemID] = merged
		}
	}
	return out
}

// mergeEqualAdjacent collapses touching segments with identical
// (plan, qty) — defensive normalization so a semantically-identical
// split never bills as two lines. Input is start-ordered (store sort)
// and non-overlapping (DB exclusion constraint).
func mergeEqualAdjacent(segs []baseSegment) []baseSegment {
	if len(segs) < 2 {
		return segs
	}
	out := segs[:1]
	for _, s := range segs[1:] {
		last := &out[len(out)-1]
		if s.planID == last.planID && s.quantity == last.quantity && s.start.Equal(last.end) {
			last.end = s.end
			continue
		}
		out = append(out, s)
	}
	return out
}
