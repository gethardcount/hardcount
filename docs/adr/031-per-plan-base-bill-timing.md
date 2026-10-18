# ADR-031: Per-plan base bill_timing (in_advance vs in_arrears)

**Status:** Accepted (amended 2026-06-01)
**Date:** 2026-05-14
**Related**: ADR-028 (period loop), `project_billing_timing_model.md`, `feedback_stripe_parity_framing.md`

## Summary

Each plan has a `base_bill_timing`: `in_arrears` (the default) bills the base fee at the end of each period, and `in_advance` bills it at the start, with a first invoice when the subscription is created. Usage always bills in arrears, because its quantity is unknown until the period ends. On a mid-period cancel of an `in_advance` plan, the unused base of a paid invoice is credited back. Amended 2026-06-01: an unpaid prebilled invoice is voided when no time was used, or else reduced to the used part with a credit note. Status: active, amended; [ADR-048](048-credit-clawback-tax-reversal.md) and [ADR-118](118-cancel-and-swap-relief-in-the-change-transaction.md) later changed how this relief is written.

## Context

Velox today bills exclusively **in-arrears**: a subscription's base fee is generated as an invoice at the END of each billing period, alongside the period's usage. The model assumes the recurring fee can wait — fine for usage-only AI inference, broken for the standard B2B SaaS shape where the platform fee is charged at the START of the period and usage is settled at the END.

This is the gap the bundle closes. Without it, the very first DP demo collapses on "did they get charged on day one?".

## Industry reference

| Vendor | Recurring bill_timing | Usage bill_timing | Mixed plan |
|---|---|---|---|
| Stripe Billing | per-price (default `in_advance` for recurring; `in_arrears` for metered) | always `in_arrears` (metered prices forced) | yes — both line types on one invoice |
| Lago | per-price (`in_advance` \| `in_arrears`) on `charges`, `pay_in_advance` flag on subscriptions | per-charge | yes |
| Orb | per-price (`fixed_fee_quantity_schedule` + `cadence`) | per-price | yes |
| Velox today | n/a — single arrears engine | always in_arrears | no — every line is end-of-period |

Stripe / Lago / Orb all converge on the same shape: **recurring is the variable; usage is structurally arrears.** You can't bill in_advance for usage that hasn't happened yet — the quantity is unknown. This isn't a configuration choice, it's a physical constraint.

So bill_timing only meaningfully applies to the **recurring fee**. That's the entire decision surface.

## Decision

Add a single column to `plans` — `base_bill_timing TEXT NOT NULL DEFAULT 'in_arrears' CHECK (base_bill_timing IN ('in_advance', 'in_arrears'))`. Plumb it through the domain model, store, service, handler, and (later in the bundle) the billing engine.

- **Default `in_arrears`** preserves every existing tenant's behaviour. Migration 0084 is purely additive.
- **`in_advance`** triggers two engine paths the bundle will land in the next slices:
  - **First-invoice-on-create**: on subscription creation, immediately generate an invoice for the base fee covering the upcoming period. Usage lines on this invoice are zero (no events yet).
  - **Cancel proration**: on cancel mid-period, the unused portion of an already-billed in_advance base is refunded as a credit note (Stripe parity — Stripe's behaviour with `proration_behavior: create_prorations`).
- **Usage stays arrears-only.** Meters are not granted a bill_timing knob — the schema doesn't model what would be invalid. A future "minimum commit / drawdown" feature (an AI-native wedge item) is a different shape and gets its own ADR.

### Why plan-level, not price-level

Velox's pricing model doesn't have a first-class "Price" entity per se. A plan has:
- One `base_amount_cents` (the recurring fee)
- N meter IDs (usage)

This is the layer where the in_advance / in_arrears choice lives. Lago / Orb model it per-price because their data shape is per-price; ours is per-plan with two natural "rows" (base + usage[]). The semantics line up: bill_timing is a property of the base row only.

If the future demands per-line bill_timing (e.g. one-off setup fees with their own timing), the migration path is to split the `base` row into a first-class `plan_prices` table with `bill_timing` per row. Today's column is forward-compatible — `base_bill_timing` becomes the value for the canonical "base" row in that future schema.

### Why not a `billing_mode` enum with more values

Considered: `billing_mode IN ('arrears_only', 'advance_base_arrears_usage', 'advance_only')`. Rejected because `advance_only` is incoherent — you can't bill the platform fee in advance and ignore usage forever; usage still has to settle. The two-value boolean shape (`in_advance` | `in_arrears` for the recurring base, with usage implicit) is the minimum cardinality.

## Consequences

**Positive:**
- Velox can demo the **hybrid B2B SaaS shape** (in_advance base + in_arrears usage on one invoice) — the 60%+ pattern in self-serve and mid-market deals.
- One Stripe-parity gap closed without taking Lago/Orb's full per-price schema cost.
- Forward-compatible: if per-price granularity is later required, the existing column becomes the value for the canonical base row.

**Negative:**
- Engine work fans out: first-invoice-on-create, cancel proration, plan-change proration when base_bill_timing differs, dunning interaction when the in_advance invoice fails on day 1. Each is bundled in subsequent slices of this workstream.
- MANUAL_TEST flows B1 / B6 / B7 / I11 silently assume arrears-only — they'll be rewritten in the bundle's last slice. Until then, those flows test ONE shape (the historical default) and don't yet cover in_advance.
- `bill_timing=in_advance` plans require an active payment method at sub creation for the first-invoice flow to land smoothly. The auto-charge path already handles "no PM" by leaving the invoice in `auto_charge_pending=true` with an email — same behaviour applies on day 1.

**Neutral:**
- Webhook events: no new event types. `invoice.created` on day 1 already covers the in_advance case; the consumer can't distinguish "cycle invoice" from "create invoice" without `billing_reason`, but `billing_reason` already exists and gains a new value `subscription_create` in this bundle's later slice (engine slice).

## Migration

`0084_plans_base_bill_timing` adds the column with default `'in_arrears'` so every existing row keeps its current behaviour. Backout is a clean `DROP COLUMN IF EXISTS base_bill_timing` (a sub created with `in_advance` and billed under the new path would have a credit-note proration on cancel; rolling the schema back wouldn't unwind ledger entries, but the data stays valid because the new code path is silent under the default).

## Amendment 2026-06-01: cancel relief when the in_advance prebill is UNPAID (velox-ops #22)

This ADR's "Negative" list flagged "dunning interaction when the in_advance invoice fails on day 1" as a deferred slice. This amendment resolves it.

**Problem.** `BillOnCancel` only grants the unused-portion credit when the source in_advance invoice was *paid* (correct — you don't gift a customer balance they never funded). But when the invoice was **unpaid**, it did nothing: the full-period invoice stayed open and rode the dunning path. A customer who cancels on day 3 of a 30-day prebilled period was still chased for all 30 days.

**Industry research (2026-06-01).** Verified across Stripe, Orb, Lago, Chargebee, Recurly, Metronome. Convergent rule (4 high-confidence platforms — Stripe, Orb, Lago, Chargebee): on cancel of an unpaid prebill, **settle the invoice down to the consumed portion** — never chase the full amount, never issue an unfunded credit. Two mechanizations: reduce-to-consumed via an adjustment credit note (Chargebee/Orb/Lago) or void-and-reissue (Stripe). AI-native peers (Orb, Lago) reinforce reduce-to-consumed and gate the credit-note type on payment status — the same gate Velox already had at the credit-grant level. The prior leave-in-dunning behavior matched no platform.

**Decision.** Extend `BillOnCancel`'s unpaid branch to act on the invoice itself:
- **Paid** → unchanged: grant the unused amount to the customer's credit balance.
- **Unpaid, nothing consumed** (whole remaining receivable unused, nothing paid) → `invoice.Void()` (clean terminal status, full tax reversal). Matches Orb's pure-upfront void.
- **Unpaid, partially consumed** → adjustment credit note reducing `amount_due` to the consumed portion (reuses `creditnote.Service`; reverses the unused fraction's tax). The net unused base is grossed up by the invoice's `Total/Subtotal` ratio so the credit note covers the unused portion's tax too. No customer-balance credit (no cash was funded).
- Partial payment present → never void (would annul collected money); reduce instead, clamped to `amount_due`.

**Chose reduce-via-credit-note over void-and-reissue** for the partial case: identical economics, reuses an existing concurrency-hardened primitive (vs synthesizing a new invoice + re-running tax), keeps one invoice for the customer's paper trail, and matches 3 of 4 high-confidence platforms.

**Dunning posture:** the consumed-portion residual stays collectible (it is legitimately owed); dunning's existing terminal `mark_uncollectible` covers the eventual-give-up case. Not paused on cancel.

**Scope held for v1** (per pre-launch scoping): hardcoded default, no `Skip/Reduce/Void/Refund` tenant setting (Chargebee/Lago expose one — revisit on a named DP request). Idempotency relies on `CancelAtomic` refusing to re-cancel an already-canceled sub, so `BillOnCancel` runs once. No new webhook event types; `invoice.voided` / credit-note events from the underlying services cover it. Wiring: `engine.SetInvoiceVoider` / `engine.SetCreditNoteAdjuster` (narrow domain-typed interfaces, mirroring `SetCreditGranter`).
