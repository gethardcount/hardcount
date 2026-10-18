# ADR-038: Credit notes use three explicit allocation channels (Stripe + Lago shape)

**Date:** 2026-05-24
**Status:** Accepted

## Summary

A credit note (a document that reduces or refunds an invoice) on a paid invoice splits its amount across three channels that must add up to its total. `refund_amount_cents` goes back to the card through Stripe, `credit_amount_cents` goes to the customer's credit balance, and `out_of_band_amount_cents` records money refunded outside Velox. The refund part is capped at what was paid minus earlier refunds, and a note that names no channel goes wholly to credit balance. On an unpaid invoice the channels are ignored and the note reduces the amount due. Status: active; the older single-channel `refund_type` field still works.

## Context

Velox's credit-note dialog presented a binary `Type: Credit | Refund` dropdown. When the operator picked Refund on a paid invoice with a mix of card and credit-balance payment, the server's auto-routing logic silently allocated part of the refund to credit balance — which contradicted the explicit UI choice. We initially shipped a strict-reject (2026-05-22) that flagged subtotal > pmRefundable and refused the request; the operator then had to either lower the amount, pick the other type, or split into two CNs. The strict-reject created the symptom in a live demo: an operator hit it, gave up, and used Credit type for an $80 CN where $62.60 should have gone to the card.

A user-driven question — "doesn't look complete auto-split is it?" — prompted a research pass across the major billing-engine peers to determine the actual industry pattern.

**Verified industry behaviour (2026-05-24, multi-platform WebFetch):**

- **Stripe** ([Create credit note API](https://docs.stripe.com/api/credit_notes/create)): three explicit allocation parameters — `refund_amount`, `credit_amount`, `out_of_band_amount`. *"The sum of refunds, customer balance credits, and outside of Stripe credits must equal the post_payment_amount."* Dashboard shows the three as mutually exclusive single-channel choices, but the API requires explicit allocation.
- **Lago** ([Create credit note API](https://getlago.com/docs/api-reference/credit-notes/create)): `refund_amount_cents`, `credit_amount_cents`, `offset_amount_cents`. *"The refunded, credited and offsetted amounts should always balance. The total, including taxes, cannot exceed the invoice's total fees."*
- **Chargebee** ([Credit Notes docs](https://www.chargebee.com/docs/2.0/credit-notes.html)): type determined by invoice status (paid → Refundable, unpaid → Adjustment, standalone → Promotional). No split within one CN.
- **Orb** ([Credit notes docs](https://docs.withorb.com/invoicing/credit-notes)): paid-invoice CN always increments customer balance. For cash refunds, operators process the refund outside Orb and manually decrement the balance.
- **Recurly** ([Adjustments docs](https://docs.recurly.com/docs/adjustments)): operator picks ordered priority — "Credit first" (default) or "Transaction first". System applies sequentially.
- **Metronome** ([Credit memos docs](https://docs.metronome.com/invoice-customers/issue-credit-memos/)): no refunds supported; credit memos route to future billing only.

**Convergence reading:** Stripe + Lago are the closest peers to Velox (engine-style, API-first, multi-channel refund routing). Both expose three explicit allocation fields with a sum invariant. Chargebee + Orb pick a single channel per CN. Recurly auto-orders. Metronome opts out. The Stripe + Lago shape is the dominant pattern in the Velox peer set.

## Decision

Velox `POST /v1/credit_notes` accepts three explicit allocation fields:

- `refund_amount_cents` — refunded to the original payment method via `payment.Refunder.CreateRefund` (Stripe today)
- `credit_amount_cents` — granted to the customer's credit balance via `credit.Service.GrantForCreditNote`
- `out_of_band_amount_cents` — recorded but not actioned (operator handled the refund externally — cash, ACH, manual adjustment)

For paid invoices the three amounts must sum to the CN total. `refund_amount_cents` is capped at `AmountPaidCents − prior CN refund_amounts` (the PM-refundable cap). Negative amounts are rejected.

For unpaid invoices the allocation is ignored — the CN reduces `amount_due_cents` directly.

The legacy `RefundType` field (`"refund"` | `"credit"`) is kept for back-compat and translated into the equivalent single-channel allocation when the three explicit fields are all zero. Existing SDK callers don't break.

Default routing when none of the three is specified and `RefundType` is empty: `credit_amount_cents = total` (Orb-style — no cash movement, restorable later, safest fallback).

`internal/domain.CreditNote` gains `OutOfBandAmountCents int64`. Migration **0094** adds the column to `credit_notes` with `BIGINT NOT NULL DEFAULT 0 CHECK (>= 0)`.

Dashboard `IssueCreditDialog` (in `web-v2/src/pages/InvoiceDetail.tsx`) replaces the Type dropdown with three amount inputs, a live "Allocated $X / $Y ✓" indicator, an auto-balance helper (typing in Refund auto-fills Credit with the remainder), and a Save gate that blocks until allocation balances and refund stays within the PM-refundable cap. CN list view (`CreditNotes.tsx`) labels rows with the actual channel breakdown ("refund" / "credit" / "refund + credit" / "refund + credit + out of band"). CSV export adds Refund/Credit/Out-of-band columns.

## Why this design

- **Industry convergence within the Velox peer set.** Stripe + Lago are the engine-style peers; both ship the three-channel explicit-allocation shape. `feedback_peer_set_matters` — Orb / Metronome / Lago are the right reference set for Velox, not Stripe alone.
- **Removes the silent-contradiction failure mode.** Either the operator picks the allocation (no surprise) or they accept the default (full credit balance, lowest-friction reversal). The strict-reject UX failed because it pushed the friction onto the operator without giving them the tools to resolve it in-dialog.
- **Strictly more powerful than the previous binary.** Credit-type CN is just `credit_amount = total`; Refund-type is just `refund_amount = total`. Anything the old shape could express, the new shape can. The new shape also expresses the mixed-payment split that previously required two separate CNs.
- **Aligns with `feedback_stripe_parity_framing`.** Velox doesn't pick "Stripe vs Lago" — it adopts the superset that contains Stripe's behaviour as a configuration. Operators who want Stripe-style PM-first allocation can set `refund_amount = min(total, pmRefundable)` themselves; the form auto-balance does this by default.

## Alternatives considered

- **A. Restore unconditional auto-split (Stripe customer-balance docs language).** Default `refund_amount = min(total, pmRefundable)`, excess to credits, regardless of operator intent. Rejected: contradicts the Velox UI's role as the operator's source of truth — the operator should be able to override the routing for the mixed-payment case (e.g. courtesy credit even when card was used). Stripe's actual API doesn't auto-split either; the docs language describes one example flow, not the API behaviour.
- **B. Keep strict-reject + add inline UX nudge.** Add a "Switch to Credit" button on the reject error. Rejected: still binary-type per CN. Doesn't address mixed-payment split in one CN. Higher operator friction for the same outcome the three-channel shape achieves in one click.
- **C. Chargebee-style status-determined type.** Paid → Refundable, unpaid → Adjustment. Operator has no per-CN choice. Rejected: too rigid for engine-style use cases (Velox is the operator's billing API; they need control over allocation). Also forces the mixed-payment case into two CNs.
- **D. Recurly-style ordered priority modes.** Operator picks "credit first" or "transaction first" globally. System applies. Rejected: hides the routing decision in a setting page. Less transparent than per-CN explicit fields. Operator can't override per-CN for special cases.
- **E. Pro-rata auto-split based on original payment composition.** Distribute the CN amount between PM and credits by the original payment ratio (e.g. 75.8% PM / 24.2% credits). Rejected: no industry precedent in the peer set; harder to reason about than Stripe's PM-first cap; loses information when the original composition isn't a simple ratio.

## Consequences

### Positive

- Operator gets exactly the routing they specify; no silent contradiction.
- Mixed-payment refunds resolve in one CN, one dialog. Previously required two CNs or a confusing error.
- API parity with Stripe + Lago. Engineers reading Stripe docs find familiar fields.
- Out-of-band channel records cash / ACH refunds in the same audit log as Stripe refunds — no separate workflow.
- Strictly more powerful than the binary; no expressiveness lost.

### Risks / open items

- **Schema migration 0094** adds a column with a default value. No data backfill; existing CNs have `out_of_band_amount_cents = 0`. Reversible via the matching `.down.sql`.
- **Legacy `RefundType` field surface** remains on the create payload but is no longer the primary path. Removing it later means a breaking SDK change; the back-compat translation is a permanent shim until v1.0 cleanup.
- **PDF rendering** still treats CN total as the single amount line. The three-channel breakdown is only surfaced in the dashboard, not on the customer-facing PDF. Acceptable: tax authorities care about the total, not the routing. Defer until a customer asks.
- **Operator confusion if they don't read the labels.** "Outside Stripe" is unfamiliar. Tooltip + plain-language label ("cash, ACH, manual") mitigates. Re-evaluate after first design-partner feedback.

## References

- Migration: `internal/platform/migrate/sql/0094_credit_notes_out_of_band_amount.up.sql`
- Service: `internal/creditnote/service.go::Service.Create` (allocation validation + legacy translation)
- Dialog: `web-v2/src/pages/InvoiceDetail.tsx::IssueCreditDialog`
- Memories: `feedback_verify_stripe_parity_claims`, `feedback_peer_set_matters`, `feedback_stripe_parity_framing`, `feedback_no_belt_and_suspenders`
- Stripe: [Create credit note API](https://docs.stripe.com/api/credit_notes/create)
- Lago: [Create credit note API](https://getlago.com/docs/api-reference/credit-notes/create)
