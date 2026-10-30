# A refund made in Stripe

A refund made in Stripe is one made outside Hardcount: in the Stripe dashboard,
or with Stripe's API called directly. This page is for checking what Hardcount
did with one, and for finishing the ones it left to you. Back to the
[runbook index](index.md).

## Symptom

One of these:

- An invoice shows the notice "A refund of … was made in Stripe and not
  recorded here".
- Your system receives the webhook event `payment.refunded_in_stripe`.
- You refunded a payment in the Stripe dashboard and want to know what
  Hardcount recorded.

## Why

- **A credit note is how Hardcount knows money went back.** A credit note is a
  document that lowers what a customer owes on an invoice, or records money
  given back after it was paid. The invoice page, the customer's invoice page,
  the PDF, the refund limit and the Stripe Tax reversal all read credit notes.
- **Hardcount writes the credit note itself when the refund is plain,** once
  Stripe says it succeeded (a pending refund waits). A
  refund of the payment that paid a paid invoice, in the invoice's currency,
  that fits what the invoice has left, and that Hardcount hears of within a
  day, becomes an issued credit note. Its card refund is that refund, so
  Hardcount calls nothing at Stripe for it, and it reverses its share of the
  tax. The note carries `recorded_from_stripe: true` and names the refund in
  `stripe_refund_id`; its row on the invoice's Activity list reads "Refunded
  in Stripe".
- **Anything else is flagged, never documented.** Hardcount does not guess:
  an issued credit note cannot be undone. A refund of another payment (a
  second charge), or one that failed, shows on the invoice's Activity list
  only. A refund of the invoice's own payment that does not fit shows the
  notice, with the reason.
- **Hardcount hears of refunds only if Stripe sends them.** The Stripe webhook
  endpoint needs `refund.created`, with the other refund events. The
  Hardcount settings page lists them under the webhook address. An endpoint
  set up before this release lacks `refund.created`: in Stripe open
  **Developers → Webhooks**, open the endpoint, and add it. Without it,
  Hardcount sees most card refunds only through their later `refund.updated`,
  more than a day after they were made, and flags them instead of recording
  them.
- **Hardcount sends the customer nothing.** Stripe emails a refund receipt
  if you turned on **Email customers for refunds** in Stripe. You can send
  the credit note yourself with **Send**.

## Check

List the refunds made in Stripe on the invoice:

```bash
curl -s "$API/v1/invoices/$INVOICE_ID/stripe-refunds" -H "Authorization: Bearer $KEY"
```

**You see:** one entry per refund, with `stripe_refund_id`, `amount_cents`,
`status` (Stripe's) and `outcome`:

| `outcome` | Meaning | To do |
|---|---|---|
| `awaiting` | Stripe has not said yet whether it went through | Nothing: Hardcount decides it on Stripe's next event |
| `credit_note` | Recorded as the credit note `credit_note_id` | Nothing |
| `other_payment` | A refund of a payment that did not pay this invoice | Nothing: that money was never on the invoice |
| `not_refunded` | It failed or was canceled before it succeeded | Nothing: no money moved |
| `needs_review` | Not recorded; `review_reason` says why | See Fix |
| `dismissed` | You marked it handled outside Hardcount | Nothing |

## Fix

**In the dashboard,** open the invoice. The "Refunded in Stripe" notice has
a button for each step below that applies to the refund, and each asks
before it acts:

- **Record as credit note** (step 1). Offered on a paid invoice when the
  reason is `seen_late`, `invoice_not_paid` or `credit_note_refund_failed`.
- **Dismiss** (step 2), with a note saying how you handled it. Always
  offered.
- **Attach to** the credit note whose refund failed (step 3). Offered for
  `credit_note_refund_failed` when that note's refund amount is the
  refund's.

With several refunds flagged on one invoice, each gets its own row of
buttons. On an invoice whose payment Hardcount also flagged, the notice is
titled "Payment needs review" and carries the same buttons after its own. A note recorded either way shows the tag "Recorded from Stripe" on
the Credit notes page and under the invoice, with the refund's id.

The same steps through the API follow. Act on a `needs_review` refund by its
`review_reason`:

| `review_reason` | Do this |
|---|---|
| `credit_note_refund_failed` | A credit note on the invoice has a card refund that failed. If this refund replaces it, attach it to that note (step 3). Otherwise record it (step 1) or dismiss it (step 2). **Retry refund** on that note is refused until then, so the customer is not refunded twice. |
| `seen_late` | If no credit note on the invoice records it yet, record it (step 1). If you already recorded it by hand, dismiss it (step 2). |
| `invoice_not_paid` | Check the invoice's payments in Stripe. Once the invoice is paid, record it (step 1); otherwise dismiss it. |
| `exceeds_card_refundable` | The refund is more than the card payment had left after the credit notes on the invoice. Check the payment in Stripe, record the difference in your own books, and dismiss it. |
| `exceeds_creditable` | An earlier credit note already gave part of the invoice back another way, for example as account credit. If that credit was not meant as well, take it back from the customer's balance. Then dismiss it. |
| `prepaid_commit` | The invoice bought prepaid credit, which the customer can still spend. Retire it with commit relief recorded as money returned outside Hardcount, as in [Card disputes](card-disputes.md#fix) step 3, then dismiss it. |
| `currency_mismatch` | Record it in your own books and dismiss it. |

1. **Record it with a credit note that names the refund.** Send the refund's
   amount as the card refund, and its id:

   ```http
   POST /v1/credit-notes
   {
     "invoice_id": "inv_…",
     "reason": "Refund made in Stripe",
     "auto_issue": true,
     "refund_amount_cents": 4000,
     "stripe_refund_id": "re_…",
     "lines": [{"description": "Refund made in Stripe", "quantity": 1, "unit_amount_cents": 4000}]
   }
   ```

   **You see:** `201` with the issued note, `recorded_from_stripe: true` and
   `refund_status` from Stripe. Hardcount reads the refund from Stripe first:
   it must be of this invoice's payment, not made by Hardcount, and not
   failed. Nothing goes to the credit balance on this note.
2. **Or dismiss it,** with how you handled it:

   ```bash
   curl -s -X POST "$API/v1/invoices/$INVOICE_ID/stripe-refunds/$STRIPE_REFUND_ID/dismiss" \
     -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
     -d '{"note": "Recorded in our books on 2026-10-30"}'
   ```

   **You see:** `200` with `outcome: dismissed`. The note is kept on the
   refund and in the audit log.
3. **Or attach it to the credit note whose refund failed,** when you refunded
   in Stripe what that note's refund was for. The amounts must match:

   ```bash
   curl -s -X POST "$API/v1/credit-notes/$CREDIT_NOTE_ID/attach-stripe-refund" \
     -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
     -d '{"stripe_refund_id": "re_…"}'
   ```

   **You see:** `200` with the note, its `stripe_refund_id` the refund and
   `refund_status` `succeeded`. No new note, and no second tax reversal.

**Never record a refund made in Stripe by hand** with an ordinary credit note.
One with a card refund refunds the customer again; one without leaves the
refund unlinked, and Hardcount may record it a second time. Use step 1 or 3.

**If the refund of a recorded credit note fails later** (Stripe can fail a
card refund up to 30 days on, and you can cancel a card refund in the Stripe
dashboard for a short time), the note's `refund_status` becomes `failed` and
it shows in the refunds needing attention list. The customer was not
refunded. **Retry refund** on the note makes a new refund through Hardcount:
use it only if you still mean to refund. If you canceled it on purpose, the
note stays as a record of the attempt; issue a new invoice if the customer
owes the money again. A refund canceled before it succeeded never gets a
note (`not_refunded`).

## Verify

The invoice's notice is gone, and the list shows `credit_note` or
`dismissed`. For a recorded note on an invoice with Stripe Tax, the note's
`tax_transaction_id` names the reversal.

Two log lines matter:

- `refund webhook names a payment no Hardcount invoice holds`: the payment was
  not made through Hardcount, so nothing was recorded. For the first 15
  minutes after Stripe makes a refund, Hardcount asks Stripe to send the
  event again instead, because it may not have recorded its own payment yet.
- `refund webhook carries a status Hardcount does not know`: Stripe added a
  refund status. The delivery fails, so Stripe sends it again for up to three
  days. Upgrade Hardcount to a release that knows the status.

Related: [how Hardcount handles card payments and refunds](../../payments.md),
the [webhook events](../../webhooks.md#event-catalog), and
[ADR-134](../../adr/134-refunds-made-in-stripe-are-recorded.md).
