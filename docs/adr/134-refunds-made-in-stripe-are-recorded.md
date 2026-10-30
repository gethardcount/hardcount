# ADR-134: Refunds made in Stripe are recorded as credit notes, or flagged

**Date:** 2026-10-30
**Status:** Accepted

## Summary

A refund made outside Hardcount on a Hardcount payment (in the Stripe dashboard, or with Stripe's API called directly) is now recorded. When it is plainly a refund of the invoice's own payment and fits what the invoice has left, Hardcount records it as an issued credit note. The note's card refund is that refund, so Hardcount calls nothing at Stripe for it, and the note reverses its share of the tax. Every other such refund is recorded and flagged on the invoice, never documented. The operator records it with a credit note or dismisses it. One new table, `invoice_stripe_refunds`, holds every such refund. One new outbound event, `payment.refunded_in_stripe`, is sent once per refund. This amends [ADR-063](063-refund-status-webhook-reconciliation.md) ("Deferred: ingesting external refunds") and [ADR-049](049-payment-settlement-primitive.md) ("Deferred: inbound `charge.refunded`"), and corrects ADR-049's note on why a `charge.refunded` was skipped.

## Context

An operator who refunds a Hardcount payment in the Stripe dashboard left Hardcount unaware of it. The invoice page, the hosted invoice page, the invoice PDF and the customer page still said the invoice was paid in full with nothing refunded. The customer's card statement and Stripe receipt showed the refund. Stripe Tax still held the full sale. Hardcount uses the Stripe Tax API directly, and for that integration Stripe says "When you issue a Refund, you must create a reversal Tax Transaction" (docs.stripe.com/tax/payment-intent/custom). The credit-note refund cap still counted the money as refundable, so a later Hardcount refund was accepted and then refused by Stripe (`amount_too_large`). Nothing told the operator.

In Hardcount the tax reversal and the refund cap both belong to credit notes (ADR-038, ADR-063). A credit note is also what every document reads "refunded" from. So recording a refund means writing a credit note.

What peers do (read 2026-10-30; the full table and sources are in the design, `velox-ops/research/row12-dashboard-refunds-design-2026-10-30.md`):

- **Stripe Billing** learns of a dashboard refund, caps later credit notes by it, and with Stripe Invoicing "Stripe Tax adjusts your tax liability in proportion to the refunded amount". The invoice stays `paid`.
- **Chargebee:** "Chargebee does not automatically detect or sync such externally initiated refunds." The operator uses **Record Refund**, which makes a credit note.
- **Orb:** "it's best practice to create a refund credit note in Orb … to reflect that the cash refund already occurred outside Orb."
- **Lago** ignores a refund it did not make ("Refund was not initiated by lago").

The common practice is that the invoice stays paid and a credit note carries the refund. Hardcount goes one step past the peers that leave it to the operator: it writes the note itself, for the one case where the note's content is fully known.

## Decision

1. **Hardcount listens to `refund.created`,** with the refund events it already handled (`refund.updated`, `refund.failed`, `charge.refund.updated`). Each carries the whole Refund. `charge.refunded` is not used: it names no refund, only a running total.

2. **Hardcount's own refunds are told apart by their metadata.** Every refund Hardcount creates carries `hardcount_cn_id` (the ADR-063 amendment of 2026-08-04). Such a refund is never recorded as one made in Stripe. Its status goes to the credit note holding its id. Its webhook can arrive before that id is saved: then a young refund asks Stripe to deliver it again, and an old one is acknowledged, for Retry refund to adopt. The check reads the refund itself, not the database, so no race between a read and a write can turn a Hardcount refund into an external one.

3. **A refund without that metadata is recorded by one writer,** `creditnote.Service.RecordStripeRefund`, reached from the payment webhook handler through a consumer-defined interface (`payment.ExternalRefundRecorder`). It finds the invoice from the refund's row, else from the credit note that holds the refund id, else by the refund's PaymentIntent (the dispute lookup). It locks the invoice row, then the credit-note lock, then the refund's row, and decides under them. A credit note that already holds the refund id gets the status, monotonically, as from any refund webhook.

4. **At first sighting a refund becomes a credit note only when all of these hold:**
   - it did not fail or get canceled;
   - its PaymentIntent is the invoice's own payment;
   - the invoice is paid, with payment status succeeded;
   - the currency matches;
   - the invoice bought no prepaid credit (ordinary credit notes are refused there, ADR-078 D4);
   - Hardcount sees it within 24 hours of its creation;
   - it fits both caps: what the card payment has left to refund after the notes not voided, and what the invoice has left to credit.

5. **The recorded note** is issued in the transaction that creates it, with no draft ever visible, like commit relief. It has one gross line, "Refund made in Stripe (re_…)", with the proportional tax and last-note true-up every note has. All of it goes to the card refund. `stripe_refund_id` names the refund, `refund_status` is mapped from Stripe's, and `recorded_from_stripe` is true. The reason is "Refunded in Stripe", with Stripe's reason when given. After the commit the shared external legs run: the refund leg skips because the note holds a refund id, and the tax reversal runs, recovered by its sweep if it fails.

6. **Every other refund is recorded and flagged, never documented.** The row's outcome says what happened:

   | Outcome | When | What the operator sees |
   |---|---|---|
   | `other_payment` | A refund of a payment that did not pay the invoice: a second charge, or money on a voided invoice | A timeline row. No credit note: that money was never on the invoice |
   | `needs_review` | A refund of the invoice's own payment that failed a check (the review reason says which) | The `refunded_in_stripe` notice, until recorded with a credit note or dismissed |
   | `not_refunded` | First seen already failed or canceled | A timeline row. No money moved |

   A young refund of a payment on an invoice not paid yet asks Stripe to deliver it again, because the payment's own record may be on its way. An old one is flagged `invoice_not_paid`.

7. **A refund larger than what is left to credit is flagged only.** Example: $100 paid by card, a $50 note to the credit balance, then $100 refunded in Stripe. Hardcount never writes a partial note, and never takes the credit back by itself: that would be a new money rule. The operator decides and dismisses it.

8. **One table, `invoice_stripe_refunds`** (migration 00222), one row per refund, owned by the credit-note domain. It keeps the newest snapshot by Stripe's event time. Its unique `(tenant_id, stripe_refund_id)` index is the backstop for two deliveries at once; the invoice row lock is what serializes them. It forces row-level security by tenant and mode, and cascades with its invoice for test-clock teardown, which deletes it before the clock's credit notes.

9. **One event, `payment.refunded_in_stripe`,** queued in the transaction that first records the refund, with the invoice and the refund. A later change to the refund sends nothing more. A recorded note's refund status follows it as for any refund, and a later failure follows the existing refund-failure path (the refunds needing attention list).

10. **The operator's two actions.** `POST /v1/credit-notes` takes an optional `stripe_refund_id`, and Hardcount reads the refund from Stripe before the create. The refund must be of this invoice's payment, not made by Hardcount, and not failed or canceled. The note sends exactly its amount to the card, nothing to the credit balance, and is issued at once. Its card refund is then done, with no Stripe call, and a flagged row is marked recorded. `POST /v1/invoices/{id}/stripe-refunds/{stripe_refund_id}/dismiss` records "handled outside Hardcount" with the operator's note and an audit row.

11. **Nothing else moves.** The invoice keeps its status and payment status. Dunning is never started or ended, the credit ledger is never written, and Hardcount sends the customer no email (Stripe sends its own refund receipt when the operator turned that on).

## Consequences

- The invoice page, the hosted invoice page, the invoice PDF, the customer page and the credit-note totals show a refund made in Stripe, because each reads credit notes.
- Stripe Tax is reversed for a recorded refund without anyone acting.
- A Hardcount refund after a recorded one is refused up front by the existing cap.
- An issued credit note cannot be voided, so a wrong recorded note cannot be taken back. That is why only a refund that passes every check is recorded.
- Existing Stripe webhook endpoints lack `refund.created`. Without it many card refunds are still seen through their later `refund.updated`, but then more than 24 hours late, so they are flagged rather than recorded. The release notes tell operators to add the event.
- Retry refund on a recorded note whose refund later failed reads the refund, sees it failed, and makes a new Hardcount refund. That is right when the operator asks for it.

## Rejected

- **Flag every refund and let the operator record it** (the Stripe dashboard's own flow). The documents and Stripe Tax would stay wrong until someone acts, and a forgotten flag costs real tax money.
- **A freestanding refunds model.** ADR-063 keeps a refund a leg of its credit note. The new table is an inbound log of facts from Stripe, like `invoice_disputes`, not a second refund model.
- **A `refunded` invoice payment status.** No peer has one, and credit notes already carry the fact.
- **Recording it as money returned outside Hardcount.** False: the money went back to the card through Stripe, and the card refund cap would stay wrong.
- **A partial note for the part that fits.** A document for an amount nobody refunded is wrong the other way.

## Deferred (named triggers)

- **A refund poll** for webhooks that never arrive. Trigger: an observed missed refund (ADR-063's trigger).
- **Linking a refund into commit relief.** Trigger: a tenant with prepaid credit refunds one in Stripe.
- **Clearing the anomaly notice when the extra payment is refunded in full.** Trigger: operators ask.

## Related

- [ADR-063](063-refund-status-webhook-reconciliation.md): refund status from webhooks; this ADR adds refunds Hardcount did not make.
- [ADR-049](049-payment-settlement-primitive.md): the disputes amendment is the precedent (`invoice_disputes`, one writer, any delivery order).
- [A refund made in Stripe](../ops/runbook/refund-made-in-stripe.md) and [Card disputes](../ops/runbook/card-disputes.md) for operators.
