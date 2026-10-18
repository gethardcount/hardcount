# ADR-020: Invoice timeline coalesces redundant rows and surfaces the charged card

**Status:** Accepted (partly superseded by [ADR-103](103-single-payment-source.md) on 2026-07-28: the payment-row dedup rules)
**Date:** 2026-05-04

## Summary

The "Invoice paid" row on the invoice timeline names the card that was charged, for example "via Visa •••• 4242". When a payment settles, Velox looks the card up from the PaymentIntent at Stripe and stamps `payment_card_brand` and `payment_card_last4` on the invoice, so one-off Checkout cards show too. The rules below that drop duplicate Stripe webhook rows are superseded by [ADR-103](103-single-payment-source.md): payment rows now come only from charge-attempt records. Status: active for the card detail, superseded for the timeline rules.

## Context

The invoice activity timeline was rendering implementation-detail
state transitions instead of operator-meaningful milestones. An
operator looking at a paid invoice saw:

```
Invoice paid          $29.00         May 4, 7:28 PM
Payment succeeded     $29.00         May 4, 7:28 PM
```

Two rows, same minute, same amount. The lifecycle column flip
(`invoices.status='paid'`) and the Stripe webhook
(`payment_intent.succeeded`) describe the same fact from two
angles: Velox's internal state vs Stripe's wire event. The
webhook IS the trigger; the duplication is mechanical, not
informational.

Audit across the timeline-merge surface (lifecycle + Stripe +
dunning + email) surfaced two more redundancy classes:

1. **Voided + payment canceled** — when an invoice is voided
   while a PI is pending, both rows fire (lifecycle voids the
   invoice, Stripe cancels the PI). Same fact.
2. **Dunning resolved + invoice paid + payment succeeded** —
   when dunning retries finally collect, four rows in the same
   minute say "we got paid eventually." Operator-meaningful
   single event: *"after 3 retry attempts, paid."*

Adjacent gap: the surviving "Invoice paid" row showed only the
amount — not which card was charged. Operators couldn't tell at
a glance whether collection was on the customer's primary card,
a backup card, or a one-off Checkout-session card the customer
didn't even save.

## Decision

Two coupled changes.

### 1. Server-side dedup in `paymentTimeline`

The timeline handler now suppresses redundant rows at source —
not in the UI — so the wire payload itself is honest:

- Drop Stripe `payment_intent.succeeded` when `inv.PaidAt != nil`.
  Unconditional because PI succeeded ALWAYS sets `PaidAt`
  synchronously in the same handler — they co-occur within
  milliseconds.
- Drop Stripe `payment_intent.canceled` when `inv.VoidedAt != nil`
  AND the cancel-event timestamp is within 5 minutes of
  `VoidedAt`. The window matters: PI cancel can fire
  independently (24h-expired PI without a void) and a void can
  happen long after, with no shared cause. An unconditional drop
  on "VoidedAt is set" would over-suppress the standalone cancel
  in that case. 5min covers wall-clock drift between Stripe's
  event time and the void time but doesn't bleed into separate
  operator events.
- Drop dunning `resolved` rows with `reason='payment_recovered'`
  when `inv.PaidAt != nil`. The lifecycle `invoice.paid` row
  picks up `attempt_count` from the dunning run so the operator
  still sees *"after 3 retry attempts."*
- Keep `payment_intent.payment_failed` (Stripe-only signal — no
  lifecycle counterpart).

`payment_intent.payment_failed` plus dunning `dunning_started` /
`retry_attempted` / `escalated` / `manually_resolved` all stay —
they describe distinct facts (failed attempts, escalation
choice) that the operator needs.

### 2. Capture the charged card on PI succeeded

Migration 0077 adds `invoices.payment_card_brand` and
`payment_card_last4` (both nullable). The
`payment_intent.succeeded` webhook handler now:

1. Calls existing `MarkPaid` (atomic state flip).
2. Calls the Stripe API to retrieve the PaymentIntent with
   `expand=payment_method` — so the lookup works for **one-off
   Checkout cards** the customer didn't save (the PI's
   ephemeral PM still resolves to brand + last4 via Stripe).
3. Calls new `Store.SetPaymentCard` to stamp the columns.

`MarkPaid` signature was deliberately **not changed** — too many
upstream callers (dunning resolver, public payment-page handler,
payment reconciler) that don't have card info. The card-stamp is
a separate optional update; missing values render no sub-line in
the timeline (graceful degradation).

The timeline handler reads the columns and populates a new
`Detail` field on the lifecycle `invoice.paid` row:

```
Invoice paid          $29.00         May 4, 7:28 PM
                      via Visa •••• 4242
```

### Why not extract from the Stripe webhook payload directly?

The PI succeeded payload's `data.object.payment_method` is just
the PM ID (string), not the card details. To get brand + last4
inline, the webhook endpoint would need auto-expand configured
at Stripe's side. A retrieve-with-expand is the explicit
alternative — adds ~50ms per webhook, but is robust to changes
in Stripe's webhook payload shape. Given PI succeeded webhooks
aren't high-volume per tenant, the latency is acceptable.

### Why not look up via Velox's `payment_methods` table?

That works for setup-intent-attached PMs but misses one-off
Checkout cards (the customer's "I don't want to save" choice).
Operator who pays via the hosted invoice URL without saving
would see no card detail despite a successful charge —
reported by an operator on 2026-05-04. The Stripe API lookup
covers all cases.

## Consequences

### Positive

- Timeline reads as operator-meaningful milestones instead of
  state-transition tuples. One row per fact, not two.
- The charged-card sub-line ("via Visa •••• 4242") matches what
  Stripe / Lago / Recurly all show on equivalent screens.
- Dedup is server-side: the wire payload is honest, no
  client-side coalesce logic that can drift.
- Card-stamp is best-effort: a Stripe API blip leaves the row
  rendering as "Invoice paid · $29.00" with no sub-line —
  graceful, not broken.
- `MarkPaid` signature unchanged: dunning + reconciler + public-
  payment paths keep working without churn.
- Brand normalization (visa→Visa, amex→American Express) means
  the dashboard reads the same regardless of how Stripe enums
  evolve.

### Negative

- One extra Stripe API call per `payment_intent.succeeded`
  webhook (PI retrieve with expand). ~50ms latency. Async
  webhook handler — not on a request hot path.
- `payment_card_*` columns are populated only for invoices paid
  AFTER this ADR ships. Historical paid invoices show no
  sub-line; backfill would require iterating prior PI succeeded
  events with Stripe lookups per row, which we don't do (per
  the no-speculative-backfill memory). Acceptable: future paid
  invoices show the card; old ones look the same as before.
- The card detail is captured at pay-time, not refreshed. If the
  customer later rotates their card, the historical paid invoice
  still shows the original brand/last4. Correct — that's what
  was actually charged.

## Compatibility

- API: `POST /v1/invoices/{id}/payment-timeline` response
  unchanged (existing fields). New `detail` field on each event
  — clients that ignore unknown fields work unchanged.
- New `payment_card_brand` and `payment_card_last4` fields on
  the invoice JSON. Same shape — additive.
- `MarkPaid` keeps its existing signature; new `SetPaymentCard`
  is additive.

## Deferred / future work

- **Payload-first card extraction.** When operators configure
  Stripe's webhook endpoint with `auto-expand` on the
  `payment_method` field, the PI succeeded event payload arrives
  with the card details inline — no retrieve needed. The current
  code always calls retrieve. Future improvement: parse the
  payload first, fall back to retrieve only when the inline data
  is absent. ~30 lines, opportunistic. Not load-bearing because
  the retrieve is reliable and webhooks are async (latency budget
  isn't tight).

- **Historical backfill.** Invoices paid before this migration
  show no card sub-line. Per the no-speculative-backfill memory,
  no migration tool was written. A future operator-triggered
  "backfill missing card details" command could iterate paid
  invoices, look up each PI from Stripe, and stamp the columns.
  Not done because pre-launch volume is zero and post-launch
  operators rarely care about historical cosmetic fill-in.

- **Atomic MarkPaid+SetPaymentCard.** Today they're two writes;
  a network blip between them leaves invoice paid with no card
  detail. Could fold into one write with a richer MarkPaid
  signature, but that churns three other call sites (dunning,
  reconciler, public-payment) that don't have card info. Cost/
  benefit currently favours the split.

## Notes

- The dedup branches are conditional on lifecycle columns
  (`PaidAt`, `VoidedAt`), not on timestamp matching. Handles the
  edge case where a webhook arrives 30 minutes late (network
  blip): suppression still applies because the lifecycle column
  is set. Operator's mental model is "did this invoice get
  paid?" — not "in what order did our DB and Stripe acknowledge
  it?"
- `formatPaymentCardDetail` is the single canonical formatter
  for the sub-line. Tests cover the brand normalization +
  graceful degradation paths.
- The `Detail` field is intentionally generic — future event
  types (e.g. credit-applied, refund-issued) can use it for
  their own contextual lines without further schema changes.
