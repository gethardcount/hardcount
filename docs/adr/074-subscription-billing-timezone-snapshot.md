# ADR-074: A subscription's billing timezone is snapshotted at creation, not read live

**Date:** 2026-07-04
**Status:** **Superseded by [ADR-077](077-org-level-billing-timezone.md)**
(2026-07-04). The per-subscription snapshot below is no longer the model: the
billing timezone is an ORG-level setting (default UTC), resolved live at compute
time and denormalized onto the *invoice* at issue. The deferred adversarial
panel (see "Process") is what surfaced the flaw — no peer bills each
subscription in its own frozen zone, and the snapshot's display divergence was
the sole source of a ~8-bug render class. Kept for history; do not build on it.

## Summary

This record froze a billing timezone on each subscription at creation, so that changing the org timezone would not re-time live subscriptions. It was reversed the same day: [ADR-077](077-org-level-billing-timezone.md) made the billing timezone one org-level setting (default UTC) and dropped the `subscriptions.billing_timezone` column. Its invoice change survives: each invoice stores the timezone it was issued in (`invoices.billing_timezone`), so its printed dates do not move later. Status: superseded by [ADR-077](077-org-level-billing-timezone.md).

## Context

Billing calendar date-math is anchored in a timezone (ADR-058): period
boundaries snap to civil midnight in that zone, and proration denominators
count days in it. Two halves define a subscription's calendar:

1. **The anchor DAY** — `billing_anchor_day`, persisted per-subscription
   (ADR-055) so a Jan-31 sub stably bills Jan 31 / Feb 28 / Mar 31 / ….
2. **The anchor TIMEZONE** — which, before this ADR, was read **live** from
   `tenant_settings.timezone` at every computation (`tenantLocation(ctx,
   tenantID)`), at ~23 call sites.

That split is a bug: the anchor is **half-frozen**. A subscription bills on
"the 1st," but "the 1st *in whichever timezone the tenant happens to have set
right now*." So changing the tenant timezone — a control that reads as
presentational — **silently re-times the future billing boundaries of
subscriptions that are already live**, and re-introduces exactly the
mixed-anchor drift ADR-058 exists to eliminate (a period anchored under UTC
rolling into an IST-anchored next period shifts by 5.5h, occasionally onto a
different calendar day, perturbing proration and revenue timing).

**Industry parity.** The mature model separates a *display* timezone (a lens;
changes freely, applies everywhere) from a *billing anchor* (immutable per
contract). Stripe anchors each subscription to a `billing_cycle_anchor` — a
stored Unix **instant**, never re-interpreted by a timezone setting. Chargebee
/ Zuora treat the site timezone as a **setup-time** decision that is heavyweight
(or restricted) to change once live billing exists. Velox already froze the
anchor *day*; freezing the anchor *timezone* completes the model.

## Decision

1. **Snapshot `billing_timezone` per subscription at creation** — a new column,
   the peer of `billing_anchor_day`. `Create` stamps it once from the live
   tenant timezone (`loc.String()` — the canonical IANA name, or `"UTC"` for an
   unset/invalid tenant TZ: a concrete, immutable anchor). The pair
   `(billing_anchor_day, billing_timezone)` now fully and immutably defines the
   sub's calendar.
2. **All billing-math reads the snapshot, not the live tenant setting.** New
   `subscriptionLocation(ctx, sub)` helpers (on `Engine`, `subscription.Service`,
   and the subscription `Handler` as `subLoc`) return the sub's
   `BillingTimezone`, falling back to the **live tenant timezone only for
   legacy/unset rows** (so pre-migration subs are byte-for-byte unchanged).
   Every period-boundary and proration site was swapped from
   `tenantLocation(ctx, sub.TenantID)` → `subscriptionLocation(ctx, sub)`: 16 in
   `billing/engine.go`, 1 in `preview.go`, 2 in `threshold_scan.go`, 5 in
   `subscription/service.go` (Create is the ONE exception — it reads
   `tenantLocation` to snapshot), and **1 in `subscription/handler.go`** — the
   proration-denominator (`fullBillingCycleDays`) call that resolves the zone
   via the injected `TenantLocator`. That handler site is the subtle one: it
   uses a different mechanism than the direct `tenantLocation` calls, so only
   the full site-set audit caught it; missing it would have anchored the
   proration *denominator* to the live tenant TZ while the period *boundaries*
   used the snapshot — the exact mismatch this ADR prevents.
3. **Display stays live — but see the 2026-07-04 amendment.** As first shipped,
   the render sites kept reading the live tenant timezone ("a display lens over
   the immutable billing math"). That was correct for *event instants* but
   **wrong for the period RANGE string**: the inclusive-day conversion ("last
   day covered") is a civil-calendar step that is only meaningful in the zone the
   boundary is civil-midnight in — the snapshot. Rendering the range in a
   different (live) TZ shifts it by a day for a tenant that changed TZ. The
   amendment below narrows this decision.
4. **Migration 0133 backfills every existing subscription** with its tenant's
   *current* timezone, so in-flight subs get **zero behavior change**: their
   effective timezone at migration becomes their frozen anchor. Rows whose
   tenant has no timezone stay NULL and fall back (at read time) to the live
   tenant TZ, exactly as before.
5. **Re-anchor paths keep the original timezone.** A cross-interval plan swap or
   threshold reset recomputes the anchor *day* to "now" (ADR-055) but reads the
   sub's stored `BillingTimezone` — the sub's billing-calendar timezone is fixed
   at signup and never changes. (An explicit "re-adopt the current tenant TZ on
   re-anchor" is a possible future refinement; the immutable default is the safe
   one and is flagged for the fast-follow panel.)

**Net contract:** changing the tenant timezone is now **display-only for
running subscriptions** and a **default for new ones** — never a silent
re-timing of live contracts.

## Process

This shipped as an **Opus build with the adversarial panel as a fast-follow**,
a deliberate, noted deviation from the standing "panel before any money-path
build" rule. Justification: this is **pattern-replication, not novel design** —
it copies the already-shipped, proven `billing_anchor_day` snapshot into a
parallel `billing_timezone` column. The design risk (the part a panel de-risks)
is low; the real risk is **completeness**, which was controlled by the full
site-set audit + mutation-verified tests + edge-case coverage. A fresh-context
spec self-review ran before merge. The Fable panel remains a fast-follow to
adversarially sweep for a missed edge (backfill on unusual rows, DST
interactions) when credits return.

## Amendment (2026-07-04): period-range display follows the snapshot, not the live TZ

The original decision 3 ("display stays live") was too broad. A period **range**
label ("Jun 1 – Jun 30") is derived by snapping the half-open boundaries to their
civil dates and stepping the exclusive end back one calendar day — the ADR-058
inclusive-display step. That step is only correct in the timezone the boundary is
civil-midnight in, i.e. the sub's **snapshot** (`billing_timezone`). Rendering it
in the live tenant TZ reintroduces exactly the mixed-anchor off-by-one this ADR
exists to kill: an IST-anchored period `[May 2 00:00 IST, Jun 1 00:00 IST)` reads
`"May 2 – May 31"` in Kolkata but `"May 1 – May 30"` in New_York — a full day off
— the moment the operator changes the tenant TZ. (Empirically confirmed;
mutation-locked in `period_display_test.go`.)

**Corrected contract:**

- **Period-range strings are authored by the backend in the anchor TZ.** Invoices
  already surface `Invoice.BillingPeriodDisplay`; subscriptions now surface the
  new **`Subscription.CurrentBillingPeriodDisplay`** — computed on read via
  `FormatInclusivePeriod(start, end, subLoc(sub))`, stamped at every subscription
  response site by the handler's `respondSub`/`stampPeriodDisplay`. The wire stays
  half-open; this is a derived display field.
- **The frontend renders those strings verbatim** and no longer recomputes a
  sub's period range in the browser (which defaulted to the live tenant TZ). The
  TS `formatCivil*` mirror survives only for auxiliary labels the backend doesn't
  author (trial range, single period-end date, line-item "Covers …"), and those
  now receive the explicit `billing_timezone` rather than defaulting to live.
- **Event instants still render live.** A wall-clock timestamp ("Renews", "Next
  billing", "created at") is not a civil-day-derived range and legitimately shows
  in the operator's current TZ; those keep `formatDate`.

**Shipped in two focused PRs** (money-path diffs stay small):

- **PR1 — subscription API + frontend.** `Subscription.CurrentBillingPeriodDisplay`
  stamped at every response site; the dashboard renders it verbatim.
- **PR2 — invoice.** A denormalized **`invoices.billing_timezone`** column
  (migration 0134), copied from the sub's snapshot at every invoice-creation site
  (`billing/engine.go` ×3, `threshold_scan.go` ×1). The read-path decorator
  (`invoice/service.go` `invoiceDisplayLoc`) and the PDF/hosted fallback
  (`pdf_context.go`) render `billing_period_display` in that zone; the frontend
  line-item "Covers …" label passes `invoice.billing_timezone`. **No backfill** —
  an existing invoice's period was computed under whatever TZ was live when it was
  generated (not recoverable from stored data), so legacy rows stay NULL and fall
  back to the live tenant TZ, exactly what they display today. Only invoices
  created after 0134 render in the anchor zone. This is a display fix; the invoice
  wire (`billing_period_start/end`) is unchanged.

## Accepted residuals

- Re-anchor (swap/reset) keeps the original TZ rather than re-adopting the
  current tenant TZ (see decision 5) — deliberate; revisit if a DP needs it.
- An unset-tenant-TZ sub snapshots the concrete `"UTC"` (immutable), whereas the
  migration backfill leaves an unset-tenant sub NULL (falls back live). Both are
  correct-under-their-moment; the divergence only matters if a tenant sets a TZ
  *after* having created subs with no TZ, where the pre-existing subs correctly
  keep following live until they next re-anchor.

## Test locks (mutation-verified 2026-07-04)

Unit: snapshot-at-create (Kolkata; unset → "UTC"); `subscriptionLocation` returns
the snapshot despite a *changed* tenant setting AND the boundaries genuinely
differ (Kolkata vs New_York), so the assertion can't pass vacuously; empty-TZ
legacy row falls back to the live tenant TZ. Real-Postgres: the INSERT persists
`billing_timezone` (the memStore echoes fields — only a real round-trip catches
a dropped column, the Finding-1 `activated_at` trap) AND the snapshot survives a
real `tenant_settings.timezone` change. Mutations killed: revert
`subscriptionLocation` to the live read → immutability test fails; drop
`billing_timezone` from the INSERT → persistence test fails.
