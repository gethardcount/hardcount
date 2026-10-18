package billing

import (
	"fmt"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/money"
)

// closeBaseLine is one base-fee line a cycle close emits, with the plan it
// bills. The plan rides along so the operator preview can stamp its
// base_fee lines (PreviewLine.PlanID / BaseBillTiming) the way every other
// base_fee producer does.
type closeBaseLine struct {
	line domain.InvoiceLineItem
	plan domain.Plan
}

// closeBaseFeeLines returns the base-fee lines the cycle close bills for
// [periodStart, periodEnd]. Moved out of buildLineItems unchanged so the
// close and the operator preview (previewCycle) run ONE copy of the
// proration math: a calendar stub, a mid-period item add, a quantity change
// or a plan swap prorates in the estimate exactly as on the invoice
// (M-SUB-1, 2026-10-16 — the preview used to show the full base fee).
//
// segsByItem is windowSegments over the period; plans must already hold
// every plan a segment references. terminalCycleClose skips the upcoming
// in_advance base when a scheduled cancel fires at this boundary;
// thresholdBilled skips the in_arrears base when a reset=false threshold
// invoice already billed it this cycle.
func closeBaseFeeLines(sub domain.Subscription, segsByItem map[string][]baseSegment, plans map[string]domain.Plan, periodStart, periodEnd time.Time, invoiceCurrency string, billingLoc *time.Location, terminalCycleClose, thresholdBilled bool) []closeBaseLine {
	var out []closeBaseLine

	// Detect partial period once — same across all items since they share the
	// billing period. Use math.Round, not int truncation: a sub created
	// at 14:00 was previously billed for 30/31 days because hour-precise
	// duration / 24 truncated to 30. With period boundaries snapped to
	// 00:00 in tenant TZ (subscription.Service), elapsed hours land on
	// (or within ±1h of) a whole-day multiple — DST transitions
	// account for the ±1h tolerance, which Round absorbs.
	periodDays := roundDays(periodEnd.Sub(periodStart))
	emitSeg := func(seg baseSegment, plan domain.Plan) {
		var one []domain.InvoiceLineItem
		var ignored int64
		emitBaseSegmentLine(seg, plan, periodStart, periodDays, invoiceCurrency, billingLoc, sub.BillingTime, &one, &ignored)
		for _, li := range one {
			out = append(out, closeBaseLine{line: li, plan: plan})
		}
	}

	// Base fee line item — segment-aware for in_arrears, single-line
	// for in_advance (which bills the upcoming period at the post-swap
	// plan; segments don't apply because the previous period was
	// already pre-paid).
	//
	// ADR-031 + segment-aware (Lago/Chargebee/Orb):
	//   - in_arrears: one line per segment within [periodStart, periodEnd].
	//     Multi-segment when the customer changed plan or quantity
	//     mid-period; single-segment otherwise. Each segment bills at
	//     its own rate × (segment_days / cycle_days).
	//   - in_advance: one line for the UPCOMING period at the current
	//     (post-swap) plan. The just-elapsed period's base was paid
	//     upfront at the previous boundary.
	itemsByID := map[string]domain.SubscriptionItem{}
	for _, it := range sub.Items {
		itemsByID[it.ID] = it
	}

	// Pass 1: items currently on the sub.
	for _, it := range sub.Items {
		plan := plans[it.PlanID]
		isAdvance := plan.BaseBillTiming == domain.BillInAdvance

		if isAdvance {
			if terminalCycleClose {
				// Sub cancels at this boundary — don't pre-pay a period
				// that won't be used. Usage on this invoice still
				// captures the just-elapsed consumption.
				continue
			}
			if plan.BaseAmountCents <= 0 {
				continue
			}
			baseStart := periodEnd
			// In_advance base-fee line item label MUST match what the
			// engine will use as the next billing period — uses the
			// billing_time-aware helper so calendar+monthly subs show
			// the calendar-aligned next period on the line item, not
			// the day-of-month-preserved drifted period.
			baseEnd := domain.NextBillingPeriodEnd(periodEnd, sub.BillingTime, plan.BillingInterval, billingLoc, sub.BillingAnchorDay)
			baseFee := plan.BaseAmountCents * it.Quantity
			description := fmt.Sprintf("%s - base fee (qty %d)", plan.Name, it.Quantity)
			// Prorate when the upcoming period is shorter than a full
			// plan cycle — e.g. the calendar-snap stub produced after
			// a yearly→monthly plan-change cycle close (new period =
			// `(yearly_end, first-of-next-month)`, 1-30 days). Pre-fix
			// this billed the full monthly base for a 7-day stub.
			// Same shape as BillOnCreate's proration and
			// emitBaseSegmentLine's segDays/fullCycleDays gate.
			advanceDays := roundDays(baseEnd.Sub(baseStart))
			// Absorb a sub-day upcoming period (ADR-091): an org-timezone
			// change (ADR-077, resolved live each roll) can re-time the next
			// boundary onto a sub-day "seam" a few hours past periodEnd. An
			// in_advance base fee is length-insensitive, so billing one for a
			// <1-day window overcharges a full cycle — and roundDays makes the
			// window 0 days, so the proration below silently no-ops and bills
			// full. Skip the line so a cosmetically-motivated org-timezone change
			// never bills a phantom period; the sub re-aligns to clean boundaries
			// on the next full cycle. Mirrors emitBaseSegmentLine's `segDays <= 0`
			// in_arrears absorb (that path was already seam-safe).
			if advanceDays <= 0 {
				continue
			}
			fullCycleDays := roundDays(advanceBillingPeriod(baseStart, plan.BillingInterval, billingLoc, sub.BillingAnchorDay).Sub(baseStart))
			// An org-timezone change (ADR-077, resolved live each roll) re-resolves
			// the stored boundary in the new zone, leaving baseStart OFF the anchor
			// grid.
			// For anniversary/yearly, advanceBillingPeriod re-anchors to the next
			// anchor DAY, so fullCycleDays collapses to the seam width (==
			// advanceDays) and the gate below dies — a ~24h seam would then bill a
			// FULL base fee (the anniversary/yearly analogue of the calendar seam,
			// ADR-091). When baseStart is off-anchor, measure the full cycle
			// NOMINALLY (one calendar interval from baseStart, anchorDay 0) so the
			// short seam prorates instead of overbilling. Calendar billing
			// (anchorDay 0) is reported on-anchor and untouched — its denominator
			// already never collapses (it snaps to the fixed 1st-of-month grid).
			if !domain.IsPeriodStartOnAnchor(baseStart, sub.BillingAnchorDay, billingLoc) {
				fullCycleDays = roundDays(advanceBillingPeriod(baseStart, plan.BillingInterval, billingLoc, 0).Sub(baseStart))
			}
			if advanceDays > 0 && fullCycleDays > 0 && advanceDays < fullCycleDays {
				baseFee = money.RoundHalfToEven(plan.BaseAmountCents*it.Quantity*int64(advanceDays), int64(fullCycleDays))
				description = fmt.Sprintf("%s - base fee (qty %d, prorated %d/%d days)", plan.Name, it.Quantity, advanceDays, fullCycleDays)
			}
			unitAmount := plan.BaseAmountCents
			if it.Quantity > 0 {
				unitAmount = money.RoundHalfToEven(baseFee, it.Quantity)
			}
			baseStartCopy := baseStart
			baseEndCopy := baseEnd
			out = append(out, closeBaseLine{plan: plan, line: domain.InvoiceLineItem{
				LineType:           domain.LineTypeBaseFee,
				Description:        description,
				Quantity:           it.Quantity,
				UnitAmountCents:    unitAmount,
				AmountCents:        baseFee,
				TotalAmountCents:   baseFee,
				Currency:           invoiceCurrency,
				BillingPeriodStart: &baseStartCopy,
				BillingPeriodEnd:   &baseEndCopy,
			}})
			continue
		}

		// in_arrears: emit per-segment lines — UNLESS a threshold invoice
		// already billed this cycle's base. fireThreshold bills the FULL
		// (unprorated) in_arrears base for every item on the sub at fire
		// time, so re-emitting any segment here double-bills it. Known
		// imprecision: an item added AFTER the fire wasn't on the
		// threshold invoice and is under-billed by its partial-period
		// base — accepted; the alternative (segment-level reconciliation
		// against the threshold invoice's lines) over-bills in the
		// mirrored cases, and under-billing is the safer failure for a
		// rare same-cycle combo.
		if thresholdBilled {
			continue
		}
		segments := segsByItem[it.ID]
		for _, seg := range segments {
			segPlan, ok := plans[seg.planID]
			if !ok || segPlan.BaseAmountCents <= 0 {
				continue
			}
			// Skip segments whose plan was in_advance — that portion of
			// the period was already prepaid (BillOnCreate / prior cycle
			// close) and emitting a segment line here would double-bill
			// it. Matches the Pass 2 guard for removed in_advance items
			// below. Lets cross-cadence plan-swaps (in_advance OLD →
			// in_arrears NEW) bill correctly: the OLD prepaid segment
			// is skipped, the NEW in_arrears segment bills its consumed
			// portion at the NEW rate.
			if segPlan.BaseBillTiming == domain.BillInAdvance {
				continue
			}
			emitSeg(seg, segPlan)
		}
	}

	// Pass 2: items removed mid-period (in the segment map but not on
	// sub.Items now). Bill the pre-remove segments at their own rates.
	// in_arrears only — in_advance items removed mid-period already
	// paid upfront for the period; refund flows through the cancel-
	// proration / removed-item credit path (not this loop).
	// Skipped when a threshold invoice billed this cycle — same
	// double-bill guard as Pass 1 (an item removed after the fire had
	// its full base on the threshold invoice).
	for itemID, segments := range segsByItem {
		if thresholdBilled {
			break
		}
		if _, stillPresent := itemsByID[itemID]; stillPresent {
			continue
		}
		for _, seg := range segments {
			segPlan, ok := plans[seg.planID]
			if !ok || segPlan.BaseAmountCents <= 0 {
				continue
			}
			// Removed-item segments are always in_arrears-style billing
			// (period consumed before remove). in_advance items would
			// have been refunded via the cancel-proration credit path,
			// not billed here.
			if segPlan.BaseBillTiming == domain.BillInAdvance {
				continue
			}
			emitSeg(seg, segPlan)
		}
	}
	return out
}
