# ADR-012: Day-Grade Calendar Billing & Period Boundary Snapping

**Date:** 2026-05-01
**Status:** Accepted (amended 2026-07-26, 2026-10-22)

## Status
Accepted

## Date
2026-05-01

## Summary

Billing period boundaries snap to midnight in the tenant timezone, so a subscription started at any time of day is billed for that whole day. Proration counts whole days and rounds hour counts, so a daylight-saving shift cannot add or lose a day. Amended 2026-07-26: an item added on the period's first day (in that timezone) is billed for the full period. Mid-period change proration also uses a whole-day ratio ([ADR-042](042-tax-rate-decimal-precision.md)), and the timezone is the org billing timezone ([ADR-077](077-org-level-billing-timezone.md)). Amended 2026-10-22: the snap applies to base fees; a usage window starts no earlier than the trial end or another subscription's end on the meter. Status: active, amended.

## Context

A subscription started on create at `May 1, 2026 14:00 IST`, with
calendar-monthly billing, was producing an invoice with "prorated
30/31 days" — even though the dashboard's "Period" column displayed
the full month `May 1 → Jun 1`. The math and the UI disagreed because
they were anchored on different precisions:

- **UI**: rendered `period_start` and `period_end` as date-only.
- **Math**: `internal/billing/engine.go` computed
  `periodDays := int(periodEnd.Sub(periodStart).Hours() / 24)`. A
  14:00-on-the-1st creation produced `periodEnd - periodStart =
  30d 10h`, truncated to 30 by `int()`.

The root cause was upstream in `internal/subscription/service.go`,
which set `period_start = now` (the exact creation timestamp) for
calendar billing instead of snapping to a day boundary.

This bites at two layers:

1. **First-day billing**: a sub created at any wall-clock time on day
   N gets billed for fewer days than a sub created at midnight.
2. **DST transitions**: on a DST spring-forward, the 31-day period
   from Mar 1 → Apr 1 spans only `31d - 1h = 743h`, which
   `int(743/24) = 30`. The opposite happens on fall-back.

## Decision

### Period boundaries snap to start-of-day in tenant TZ

`internal/subscription/service.go` snaps both `period_start` and
`period_end` to `00:00:00` in the tenant's configured timezone (read
via a new `SettingsReader` interface, ADR-010-aligned with the
dashboard's `@/lib/dates` helpers). Stored as UTC.

- **Calendar billing**: `period_start = beginningOfDayInTenantTZ(now)`,
  `period_end = beginningOfMonthInTenantTZ(next month)`. A sub created
  at any time on May 1 is billed for the full month of May.
- **Anniversary billing**: `period_start = beginningOfDayInTenantTZ(now)`,
  `period_end = period_start + 1 month`.
- **Trial**: trial-end snaps the same way before the first paid
  period anchors against it.
- **`Activate` path**: same snap when the sub had no period set yet.

When the tenant timezone is unset or unreadable, falls back to UTC.
When the `SettingsReader` is not wired (e.g. unit tests), also UTC.

### Proration math counts whole days, DST-robust

`internal/billing/engine.go` uses `int(math.Round(d.Hours() / 24))`
instead of `int(d.Hours() / 24)`:

- With both endpoints snapped to `00:00`, a non-DST period produces
  an exact day-multiple — `Round` is a no-op.
- A period crossing a DST boundary produces `±1h` drift —
  `Round` absorbs it (e.g. `Round(743/24) = 31`, `Round(745/24) =
  31`). Truncation would silently miscount.

### Days are inclusive of the start day

A sub starting on May 12 sees `period_start = May 12 00:00`,
`period_end = Jun 1 00:00` → `Jun 1 - May 12 = 20 days` →
prorated `20/31 days`. The customer is billed for the full day of
May 12 even if the sub was created at 23:59. This matches **Lago**'s
calendar-day model (`22 days × $50 / 31 = $35.48` from their docs)
and **Chargebee**'s day-based billing mode.

### UI explains the math one hover away

`web-v2/src/pages/InvoiceDetail.tsx` renders proration line items
with a tooltip on the description. Surfaces "Subscription started
mid-cycle. Charge covers N of M days. Period boundaries snap to
start-of-day in tenant timezone (signup day inclusive)" — keeps the
description string itself tight while making the *why* discoverable.

## Industry references

| Platform | Default behaviour |
|---|---|
| **Chargebee** (day-based mode) | Snaps to `00:00:00.000` start, `23:59:59.999` end. |
| **Lago** | Calendar-day count, signup day inclusive. `22 days/31 × $50 = $35.48`. |
| **Recurly** | Day-based, standard proration. |
| **Stripe** | Default: timestamp-precise to the second. Operators with edge cases configure a "custom proration script" picking a rounding interval (`hour`/`day`/`week`/`month`). |

Hardcount's choice is the **Chargebee/Lago/Recurly default**, the
operator-friendly model. Stripe's timestamp-precise default is more
"correct" mathematically but produces exactly the kind of UI/math
disagreement that surfaced in the screenshot review. We adopt the
Chargebee shape and defer Stripe-style configurable rounding to a
future ADR if a tenant needs hour- or second-grade.

## Consequences

### Migration & backwards-compat

Hardcount is pre-launch (single tenant, no design partners). Existing
subs that pre-date this change keep their hour-precise period
boundaries until the next billing cycle, when snapped boundaries
take over. No backfill needed.

### Plan-change proration (mid-cycle)

A mid-period change is prorated in whole days, rounded at the change
instant. An in-advance change counts the days from the change to the period
end, rounded to whole days (`remainingPeriodRatio` in
`internal/subscription/handler.go`). An in-arrears base fee is billed per
segment at the period close: a segment's days are its two boundaries
rounded to whole days from the period start, then subtracted
(`emitBaseSegmentLine` in `internal/billing/engine.go`). So a period's
segments always add up to the period. Rounding each segment on its own
could bill 29 or 31 days of a 30-day period (#1274, fixed 2026-10-28). An
immediate cancel's final invoice uses the same rule. The two rules round
from different ends of the period, so for the same change instant they can
count one day apart: at an exact half day, or in a period that crosses a
daylight-saving change in the tenant's timezone.

### Tests

- `TestPeriod_DayGradeSnap` (new) pins the snap behaviour for
  calendar + anniversary + UTC fallback.
- `TestRunCycle_SkipsPendingChangeNotYetDue` (pre-existing) fails
  for an unrelated reason (date-hardcoded `2026-05-01` that today's
  calendar caught up to). Not regressed by this change.

### Future enhancements (deferred)

- Stripe-style configurable `proration_unit` per plan
  (`day` / `hour` / `second`).
- Per-plan `proration_behavior` (`prorate` / `none` / `credit_only`)
  to match Stripe's three-mode knob.
- Half-day inclusive at end (operator-confirmed credit on cancel
  mid-day).

## Amendment (2026-07-26): the segment-aware regression and the day-grade 'add' clamp

Segment-aware billing (2026-05-19, `itemBaseSegments`) silently
regressed this ADR's core guarantee. The `subscription_item_changes`
trigger (migrations 0029/0129) stamps every item INSERT with an `add`
row at the RAW creation instant, and because this ADR snaps the first
period start BACKWARD to the signup day's midnight, that row lands
strictly inside the cycle-close window (`changed_at > period_start`).
The segment walk read it as a mid-period add and re-prorated a full
first period — "prorated 30/31 days" on a full month, the exact
symptom in this ADR's Context. Found live on FLOW I1 (invoice
NIM-000247, $28.06 instead of $29). The in_advance day-1 path
(`buildOnCreateInvoice`) reads only the snapped period bounds and was
never affected — so the two cadences disagreed about the same period.

**Fix**: `itemBaseSegments` clamps a first-in-window `add` whose
`changed_at` falls on the period-start **calendar day (tenant TZ)** to
`period_start` itself. The predicate is day-grade policy, not
creation-row identification — no exact creation-vs-AddItem marker
exists in the schema, and none is needed: an add on the period's first
day owes the whole period regardless of which flow inserted it. The
clamp targets `period_start` itself (never `beginningOfDay`) so
threshold-reset and org-TZ-seam (ADR-091) periods that legitimately
start mid-day stay safe. The AddItem handler's in_advance immediate
proration clamps the same way (`remainingDays = totalDays` on a
period-start-day add) so both cadences charge the full period.

**Deliberate consequences**:

- A first-period immediate cancel bills from `period_start` (signup
  day counts whole) — a same-day create-then-cancel now bills 1
  day-grade day where it previously billed $0 for a consumed day.
  This also makes the in_arrears cancel amount agree with the
  in_advance cancel-credit twin (both net 1 elapsed day).
- First-invoice usage windows start at `period_start`, so signup-day
  events ingested before the creation instant (explicit past
  timestamps / backfill) now bill; base and usage line periods equal
  the invoice period on every surface.
- An operator AddItem landing on a RENEWAL period's first tenant-TZ
  day bills the full period. **Asymmetry, on purpose**: only `add`
  rows clamp — plan/quantity/remove changes keep timestamp-precise
  numerators per this ADR's "Plan-change proration" consequence.
  Mid-period adds (day 2+) keep instant boundaries (FLOW B20 pin).

**Accepted edges** (documented, not coded around):

- Same-day cancel→recreate on the same meter bills up to one day of
  base twice (when the cancel was post-noon): the predecessor's final
  invoice billed through the cancel instant and the successor's
  day-grade base re-covers the day. The morning's *usage* is no longer
  billed twice (amendment 2026-10-22, below).
- An org-TZ change between signup and first close can shift the
  calendar-day comparison so the clamp misses and that one sub still
  bills 30/31 (under-bill, ~1/31) — ADR-091 seam class, picked up by
  the deferred billing/display TZ decouple (ADR-092).

## Amendment (2026-10-22): one usage window for every closer

The snap is right for base fees and wrong for usage. Usage is
aggregated per customer and meter, with no subscription key. A window
that opens at midnight therefore reaches usage that is not this
subscription's to bill:

- **B2: trial hours.** The first paid period starts at midnight of the
  trial-end day (`firstPeriodAfterTrial`). A trial that ends at 18:00
  billed the usage from 00:00 to 18:00, which is trial usage. The same
  happens after End trial (`trial_end_at` = the instant it ended) and
  after Extend trial.
- **B4: same-day cancel and recreate.** The predecessor's final invoice
  billed the usage up to its cancel. The successor's period starts at
  midnight of the same day, so its close billed that usage again. The
  product tells operators to cancel and recreate in several places, so
  this is a real trigger, not a rare edge.

**Decision.** Every closer starts a meter's usage window at one floor,
`usageFloors`. The closers that bill by segment build the window with
one function, `meterUsageIntervals`. A window starts at the latest of:

1. its segment start, which is the period start for an item there from
   the start of the period (this ADR);
2. the subscription's trial end;
3. the latest instant, strictly inside the window, at which **another**
   subscription of the customer stopped covering the meter. That is
   where a billing interval on a plan carrying the meter ended (cancel,
   scheduled cancel, item removal, plan swap away). For a trial that
   never activated, it is the cancel (a trial writes no interval);
4. the threshold watermark, which still clamps only the window that
   additive buckets bill from (ADR-066 §4).

It ends at the earliest of the period end and the cancel instant, as
before. The closers that bill by segment are the cycle close, the
immediate and scheduled cancel, the cross-interval swap (ADR-056), the
operator preview and the live credit estimate. The threshold fire, its
item-threshold count and create_preview with a custom window do not
bill by segment. They rate one flat window per meter on the current
items, `[floor, now)`, where the floor is the same `usageFloors` start
taken over the whole period. The fire's usage lines are stamped with
the floored start. Rules stay pinned at the period start (ADR-070).

**Why "strictly inside" is enough.** `rejectMeterOverlap` keeps two
live subscriptions of one customer off the same meter. So another
subscription's coverage of the meter that ends inside this window
ended before this subscription took the meter. The only overlap is
the day-grade backdating, and the floor removes exactly that. The
rule is asymmetric on purpose: a later subscription's coverage always
ends after an earlier one's segment window ends, so it never moves the
earlier window. The predecessor keeps its morning. In the segment
closers, chains of removal, recreate and re-add partition the same way.
The flat window of the threshold fire has one window per meter, so
this holds there only while the subscription covered the meter in one
stretch (see the residual edges below).

**Unchanged.** Base fees and the `billing_intervals` writers keep the
day-grade starts. The trial-end day's base stays whole, and up to one
day of base on a same-day recreate stays accepted (above): this
amendment changes usage only. The customer usage report
(`internal/usage/customer_usage.go`) is a usage report, not an invoice,
and is not changed.

**Read-side alarm.** The doctor check that looks for one usage instant
on two live invoices is now scoped per customer, not per subscription
(`usage_billed_once_per_customer_meter_window`; it was
`usage_billed_once_per_subscription_meter_window`, ADR-115). Scoped per
subscription it could not see this double bill. Two lines of one
subscription are still paired only within one rating-rule bucket (the
ADR-066 §4 deferred max bucket overlaps the sum bucket by design). Two
lines of different subscriptions are paired across buckets: each
subscription pins its rules at its own period start, so a rule
published between the two gives them different versions for the same
events.

**Residual edges, accepted.** Each needs several changes to one
customer's subscriptions inside one period; revisit if one is seen.

- **One instant, not a cut.** The floor is a single instant, not a cut
  of every other subscription's interval. Say an older subscription
  covered the meter only for a stretch that began after midnight (an
  item added mid-period and removed the same day). A subscription
  created later that day then starts at the end of that stretch, and
  does not bill the start-day usage before the stretch. That needs an
  add and a remove on another subscription and a recreate, all on one
  day.
- **The fire's flat window after a meter round trip.** Subscription A
  removes meter M at 10:00 on day 5. B takes M at 11:00 and is
  canceled at 12:00. A adds M back on day 6. A threshold fire on A on
  day 20 opens M's window at 12:00 on day 5. A's own M usage before
  10:00 on day 5 is then on no invoice, because the cycle close clamps
  it away at the fire's watermark. Before this amendment the same chain billed
  B's hour twice. It needs a meter to leave A, cover B and return to A
  within one period, with a threshold fire after.
- **Current plan meters.** The coverage read
  (`ListCustomerMeterCoverageEnds`) takes each plan's meters as they
  are now, as `FindMeterConflicts` and the closers do. A plan's meters
  can change once no live subscription uses it. Say an operator
  cancels a subscription, edits its plan's meters, and creates another
  subscription for the customer the same day. The floor then uses the
  edited list. A meter the plan dropped is not floored, so its usage
  before the cancel is billed again. A meter it gained is floored at
  the cancel, so its usage before the cancel is not billed.
