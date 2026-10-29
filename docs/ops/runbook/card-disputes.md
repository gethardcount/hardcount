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
- **Hardcount records the dispute and changes nothing else.** It records each
  dispute against the invoice the payment paid, and sends the two webhook
  events. The invoice stays paid, no money moves and no payment retries
  start, because only you know whether the customer still owes the money.
- **Hardcount hears about disputes only if Stripe sends them.** The Stripe
  webhook endpoint must have three events turned on:
  `charge.dispute.created`, `charge.dispute.updated` and
  `charge.dispute.closed`. The Hardcount settings page lists them under the
  webhook address. In v0.9.0 it did not, so an endpoint set up from that
  list may lack them. To check, in Stripe open **Developers → Webhooks**,
  open the endpoint, and add any of the three that is missing.

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
   the dispute in Stripe and submit evidence, or accept it. Hardcount cannot do
   this for you. Do not refund the payment through a credit note meanwhile:
   Stripe refuses refunds on a payment under a chargeback, so the card
   refund fails. An inquiry can be resolved with a full refund.
2. **If it is won, do nothing.** The invoice is still paid and Stripe
   returns the money.
3. **If it is lost, decide what the invoice should say.** The bank already
   gave the money back to the customer. Choose one:
   - Record the loss with a credit note on the invoice. Put the amount
     under money returned outside Hardcount (`out_of_band_amount_cents`), never
     as a card refund: the customer already has the money.
   - Bill the customer again on a new invoice, if they still owe it.
   - Write it off in your own accounting. Hardcount cannot mark a paid invoice
     uncollectible.

   **If the invoice bought prepaid credit**, the customer can still spend
   that credit, and Hardcount refuses an ordinary credit note on the invoice.
   Retire the credit with commit relief instead. The relief amount goes
   inside `commit_relief`, as `out_of_band_amount_cents`:

   ```http
   POST /v1/credit-notes
   {
     "invoice_id": "inv_…",
     "reason": "Lost card dispute",
     "commit_relief": {"retire_all": true, "out_of_band_amount_cents": 5000}
   }
   ```

   Sent at the top level, the amount is refused with a 422. If the amount
   you send is wrong, the answer names the right one. Relief pays back at
   most what the customer paid for the unspent credit, so record the rest
   of the loss in your own books. If the credit is all spent, there is nothing to retire: bill
   again or write it off.

   **If the invoice also shows "Payment needs review"** (for example, a
   second payment), the notice names the payment the bank took back. Check
   both payments in Stripe before you refund anything: a refund of the other
   payment can give the customer the same money back twice.
4. **If it is `prevented`, check the payment in Stripe.** Stripe's dispute
   prevention settles a dispute by refunding the payment, and Hardcount does
   not record a refund made in Stripe. Record that refund with a credit
   note, as in step 3. No notice appears for this.
5. **If a lost dispute later turns into a win** (Stripe calls it a late
   win), Stripe returns the money to your balance, the disputes list shows
   `won` and the notice goes away. If you already acted on the loss, undo
   it: a credit note you issued for the loss is final, so record the money
   again on a new invoice. Create a manual invoice for the amount
   (`POST /v1/invoices` with its line), finalize it
   (`POST /v1/invoices/{id}/finalize`), then mark it paid outside Hardcount
   (`POST /v1/invoices/{id}/record-payment`). If you billed the customer
   again and they paid, they have now paid twice: refund that invoice with
   a credit note.

## Verify

The disputes list shows the final status. For a lost dispute, the invoice
has the credit note or the new invoice you chose. The lost-dispute notice
stays on the invoice, with the same text, as a record that the dispute was
lost, even after you act. Only a late win removes it.

Two log lines matter:

- `dispute webhook names a payment no Hardcount invoice holds`: the payment was
  not made through Hardcount, so nothing was recorded. For the first 15 minutes
  after Stripe opens a dispute, Hardcount asks Stripe to send the event again
  instead, because it may not have recorded its own payment yet. If it was
  a Hardcount payment, find the invoice by the `stripe_payment_intent_id` in
  the line.
- `dispute webhook carries a status Hardcount does not know`: Stripe added a
  dispute status. The delivery fails, so Stripe sends it again for up to
  three days. Upgrade Hardcount to a release that knows the status.

Related: [how Hardcount handles card payment failures](../../payments.md) and
the [webhook events](../../webhooks.md#event-catalog).
