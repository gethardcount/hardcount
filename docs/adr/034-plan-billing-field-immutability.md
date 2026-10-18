# ADR-034: Plan billing-field immutability once a live sub attaches

**Status:** Accepted
**Date:** 2026-05-15
**Related**: ADR-031 (per-plan bill_timing), ADR-005 (integer cents for money), industry-reference-first workflow

## Summary

A plan's base price (`base_amount_cents`), `base_bill_timing` and meter set can change only while no live subscription (any status except `canceled` or `archived`) uses the plan. After that, such an edit is refused with a message to create a new plan, because subscriptions read the plan at billing time and would be billed differently mid-cycle. Name, description, `tax_code` and status stay editable. Velox does not version plans or copy them onto each subscription. Status: active.

## Context

Velox plans are currently mutable in place. Active subscriptions read the plan live at billing time (`pricing.GetPlan` inside `billing.Engine.billOnePeriod`). Operator edits to a plan's `base_amount_cents`, `base_bill_timing`, or `meter_ids` therefore **silently change what existing subs bill at their next cycle close** — including potentially producing revenue gaps (an in_arrears base period that never gets invoiced because the plan flipped to in_advance mid-cycle).

This was caught while testing ADR-031: an operator flips a plan's `base_bill_timing` from `in_arrears` to `in_advance` while a mid-period sub exists; the next cycle invoice bills the upcoming period's base in_advance, but the elapsed period's base is never billed. Same shape applies to any base-price mutation.

## Industry reference

Surveyed billing platforms on this exact question:

| Vendor | Approach |
|---|---|
| **Stripe** | Price object: `unit_amount`, `currency`, `billing_scheme`, `recurring.*`, `tax_behavior` are **always immutable**. Only `metadata`, `nickname`, `active` are mutable. To change pricing: create a new Price, then optionally move subscription items to point to it (sub-level mutation, not price-level). |
| **Orb** | Same shape — Plan / Price billing-affecting fields immutable. Plan revisioning is first-class: each plan has revisions; subs are pinned to a revision. Operator publishes a new revision for new subs. |
| **Lago** | Subscription "overrides" snapshot the plan at attach time. Mutating the plan post-attach doesn't retroactively bill differently for existing subs. |
| **Chargebee, Recurly** | Plans are editable but **changes only apply to new subscriptions by default.** Active subs carry their snapshot of plan-as-created. Operator can opt into "apply to existing subs" with a separate workflow. |
| **Zuora** | Product Rate Plans are explicitly versioned. Subscriptions reference a specific version. Mutations create new versions. |

**Common pattern:** every mature billing platform either makes billing-affecting fields **always immutable** (Stripe, Orb) or **snapshots them at sub attach** (Lago, Chargebee, Recurly, Zuora). Velox is the outlier — mutable plans, live-reading engine, no snapshot, no versioning.

## Decision

Adopt a **soft-block** shape: billing-affecting fields are mutable **until any non-canceled, non-archived subscription references the plan**, then frozen.

Concretely, in `pricing.Service.UpdatePlan`:

| Field | Mutable always? | Mutable when no live subs? | Mutable when live subs exist? |
|---|---|---|---|
| `name`, `description` | ✓ | ✓ | ✓ |
| `tax_code` | ✓ | ✓ | ✓ (per-invoice snapshot at finalize protects past invoices) |
| `status` (active ↔ archived) | ✓ | ✓ | ✓ (Stripe `active` is mutable; archiving stops new subs without disrupting current) |
| `base_amount_cents` | — | ✓ | ✗ |
| `base_bill_timing` | — | ✓ | ✗ |
| `meter_ids` (set membership) | — | ✓ | ✗ |
| `currency`, `billing_interval` | — | n/a (not in UpdatePlan today) | n/a |

"Live" sub = status NOT IN (`canceled`, `archived`). Draft / active / trialing / paused all qualify — any of these can still produce future invoices, so the plan terms need to stay deterministic for them.

### Why soft-block, not "always immutable" like Stripe

Stripe's stricter rule is correct at their scale (millions of merchants, billions of price objects, can't be wrong even once). For Velox at pre-launch / first-DP scale, the soft-block balances:
- **Bug prevention**: silent billing gaps blocked once a sub attaches (the actual harm)
- **Operator UX**: typo correction on a brand-new plan ("oops, $99 not $98") works without forcing plan replacement

When a DP demands strict immutability (audit / SOC2 reasons), the upgrade path is trivial: flip the threshold from "live subs > 0" to "always." No data migration, just a one-line change.

### Why not snapshot-at-sub (Lago / Chargebee shape)

- Velox's existing data model has `subscription_items.plan_id` as a foreign key, not embedded plan terms. Switching to embedded would be a multi-day refactor (new columns, migration, engine reads, dashboard UX for "sub's effective terms vs plan's current terms" drift).
- The flexibility this buys ("edit a plan, no existing sub is affected") is duplicated by the soft-block + create-new-plan workflow at zero structural cost.
- Stripe is the canonical reference and Stripe doesn't snapshot. Snapshot-at-sub is a useful pattern for complex enterprise catalogs but isn't necessary for the AI-infra wedge.

### Operator workflow for billing changes

Today (Phase 1 of this ADR):
1. Operator clicks Edit on a plan with live subs, changes `base_amount_cents` → 422 with a clear error: "cannot change billing-affecting field(s) [base_amount_cents]: N live subscription(s) reference this plan. Create a new plan instead."
2. Operator creates a new plan with the desired terms.
3. Existing subs stay on the old plan OR migrate via the existing `subscription.ScheduleItemChange` machinery (pending-plan-change, `effective=next_period`, proration).

Future evolution (not in this ADR's scope):
- **"Save as new plan" button** in the Plan Detail Edit dialog. Modal: "This change creates a new plan. Existing subs keep their current plan. Migrate them at next cycle close?" — bulk plan-migration UX (was `planmigration` package, trimmed Phase 1; rebuild when a DP needs it).
- **Plan/Product split** (Stripe-parity full refactor): split `Plan` into `Product` (mutable: name/description) and `Price` (immutable: amount/timing/interval). Multi-price-per-product unlocks annual/monthly variants, regional pricing, etc. ~1-2 week refactor; defer until a DP needs it.

## Consequences

**Positive:**
- Silent billing gap from mid-stream `base_bill_timing` flip is closed.
- Same guard prevents revenue gaps from any base-price mutation, not just bill_timing.
- Error message is operator-actionable ("create a new plan instead") without prescriptive workflow.
- Allows typo correction on draft plans (no subs yet) — operator-friendly.

**Negative:**
- Operator with a $98 typo who already attached 1 sub before noticing must create `pro_v2` and migrate. Friction. Mitigated by "Save as new plan" UX in the next phase.
- Two paths to "change a plan's price": (a) no subs → just edit; (b) live subs → create new plan. Documented in the error message itself.

**Neutral:**
- Existing test surface: `pricing.Service.UpdatePlan` tests get new cases (block + allow paths). Net positive.
- MANUAL_TEST B7 (Plan change + proration) gets a caveat row about the guard.

## Migration

No schema change. No data migration. The guard runs at UpdatePlan call time against `subscription_items.plan_id` count. No backfill required because the rule is "going forward, billing fields can't change once subs attach" — past plan edits aren't re-litigated.
