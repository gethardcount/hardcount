# Card disputes

A card dispute, also called a chargeback, is when a customer asks their bank
to reverse a card payment. This page is for answering one, and for deciding
what a lost one means for the invoice. Back to the
[runbook index](index.md).

## Symptom

One of these:

- An invoice shows the notice "The customer's bank took back … after a lost
  dispute".
- Your system receives the webhook event `payment.dispute_opened` or
  `payment.dispute_closed`.
- Stripe emails you about a dispute.

## Why

- **The bank holds the money during a dispute.** When a chargeback opens,
  Stripe takes the disputed amount and a dispute fee from your Stripe
  balance. If you win, Stripe returns the amount. Some card networks first
  send an inquiry: a question about the payment, with a status that starts
  with `warning_`. An inquiry can close without becoming a chargeback.
- **Velox records the dispute and changes nothing else.** It records each
  dispute against the invoice the payment paid, and sends the two webhook
  events. The invoice stays paid, no money moves and no payment retries
  start, because only you know whether the customer still owes the money.
- **Velox hears about disputes only if Stripe sends them.** The Stripe
  webhook endpoint must have three events turned on:
  `charge.dispute.created`, `charge.dispute.updated` and
  `charge.dispute.closed`. They are not in the list the Velox settings page
  shows under the webhook address, so add them by hand: in Stripe, open
  **Developers → Webhooks**, open the endpoint, and add the three events.

## Check

List the invoice's disputes:

```bash
curl -s "$API/v1/invoices/$INVOICE_ID/disputes" -H "Authorization: Bearer $KEY"
```

**You see:** one entry per dispute, with `status`, `amount_cents`,
`reason`, `evidence_due_by` (the deadline to respond) and
`stripe_dispute_id`. The invoice page's Activity list shows the same
disputes. To open one in Stripe, search the Stripe dashboard for its
`stripe_dispute_id`.

The status tells you where the dispute is:

| `status` | Meaning |
|---|---|
| `warning_needs_response`, `needs_response` | Waiting for your response |
| `warning_under_review`, `under_review` | You responded; the bank is deciding |
| `won` | Closed in your favor; the money comes back |
| `lost` | Closed in the customer's favor; the bank keeps the money |
| `warning_closed` | The inquiry closed without a chargeback |
| `prevented` | Stripe stopped it before it became a chargeback |

## Fix

1. **While it is open, respond in Stripe before `evidence_due_by`.** Open
   the dispute in Stripe and submit evidence, or accept it. Velox cannot do
   this for you. Do not refund the payment through a credit note meanwhile:
   Stripe refuses refunds on a payment under a chargeback, so the card
   refund fails. An inquiry can be resolved with a full refund.
2. **If it is won, do nothing.** The invoice is still paid and Stripe
   returns the money.
3. **If it is lost, decide what the invoice should say.** The bank already
   gave the money back to the customer. Choose one:
   - Record the loss with a credit note on the invoice. Put the amount
     under money returned outside Velox (`out_of_band_amount_cents`), never
     as a card refund: the customer already has the money.
   - Bill the customer again on a new invoice, if they still owe it.
   - Write it off in your own accounting. Velox cannot mark a paid invoice
     uncollectible.
4. **If it is `prevented`, check the payment in Stripe.** If Stripe
   refunded it to resolve the dispute, record that refund with a credit
   note, as in step 3.

## Verify

The disputes list shows the final status. For a lost dispute, the invoice
has the credit note or the new invoice you chose. The lost-dispute notice
stays on the invoice as a record that the dispute was lost.

Two log lines matter:

- `dispute webhook names a payment no Velox invoice holds`: the payment was
  not made through Velox, so nothing was recorded. If it was a Velox
  payment, find the invoice by the `stripe_payment_intent_id` in the line.
- `dispute webhook carries a status Velox does not know`: Stripe added a
  dispute status. The delivery fails, so Stripe sends it again for up to
  three days. Upgrade Velox to a release that knows the status.

Related: [how Velox handles card payment failures](../../payments.md) and
the [webhook events](../../webhooks.md#event-catalog).
