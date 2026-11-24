# Card disputes

A card dispute, also called a chargeback, is when a customer asks their bank
to reverse a card payment. This page is for answering one, and for deciding
what a lost one means for the invoice. Back to the
[runbook index](index.md).

## Symptom

One of these:

- An invoice shows the notice "The customer's bank took back … after a lost
  dispute".
- A top-up shows the notice "The customer's bank is disputing this top-up's
  payment".
- Your system receives the webhook event `payment.dispute_opened` or
  `payment.dispute_closed`.
- Stripe emails you about a dispute.

## Why

- **The bank holds the money during a dispute.** When a chargeback opens,
  Stripe takes the disputed amount and a dispute fee from your Stripe
  balance. If you win, Stripe returns the amount. Some card networks first
  send an inquiry: a question about the payment, with a status that starts
  with `warning_`. An inquiry can close without becoming a chargeback.
- **Hardcount records the dispute.** It records each dispute against the
  invoice the payment paid, and sends the two webhook events. The invoice
  stays paid and no payment retries start, because only you know whether
  the customer still owes the money.
- **A loss on a top-up's own payment is recorded by Hardcount.** The bank
  returned money that paid for prepaid credit, so Hardcount writes one
  credit note for it and takes back the unused credit that money paid for
  (step 3).
- **Hardcount hears about disputes only if Stripe sends them.** The Stripe
  webhook endpoint must have three events turned on:
  `charge.dispute.created`, `charge.dispute.updated` and
  `charge.dispute.closed`. The Hardcount settings page lists them under the
  webhook address. In v0.9.0 and v0.9.1 it did not, so an endpoint set up from that
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
| `appeal_needs_response` | The bank rejected your response, and you can appeal it. Stripe sends this only when appeals are turned on. Closing the dispute now makes it `lost` |
| `appeal_under_review` | You sent your appeal; the card network is deciding |
| `warning_closed` | The inquiry closed without a chargeback |
| `prevented` | Stripe stopped it before it became a chargeback |

## Fix

1. **While it is open, respond in Stripe before `evidence_due_by`.** Open
   the dispute in Stripe and submit evidence, or accept it. Hardcount cannot do
   this for you. Do not refund the payment through a credit note meanwhile:
   Stripe refuses refunds on a payment under a chargeback, so the card
   refund fails. An inquiry can be resolved with a full refund.

   **On a top-up**, the notice "The customer's bank is disputing this
   top-up's payment" says what Stripe took, the date to respond by and the
   credit the customer can still spend. Hardcount holds none of that credit
   while the dispute is open: pause the customer's key at your gateway if
   you want the spending to stop. Your system can do it on the
   `payment.dispute_opened` event. The notice clears when the dispute
   closes.

   Refunding its unused credit (commit relief) answers `409`
   `payment_disputed` while the chargeback or your appeal of it is open.
   When one opens during the relief, Stripe refuses the card refund, and
   "returned another way" ends it. You see such a refund as a credit note
   whose `refund_status` is `failed`. Don't pay the customer back while the
   dispute is open: if the bank also decides for them, they are paid twice.
   Wait for the outcome:
   - **Won:** retry the refund (`POST /v1/credit-notes/{id}/retry-refund`).
   - **Lost:** the bank has already returned the money to the customer, so
     pay nothing. Mark the refund as returned another way
     (`POST /v1/credit-notes/{id}/mark-returned-another-way`), which ends
     it.
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

   **On a top-up, Hardcount records the loss itself,** in the same step that
   records the dispute as lost, once per dispute:
   - **One credit note** for what the bank returned, but no more than the
     top-up's credit notes still leave to return. Its money is
     `out_of_band_amount_cents` (the bank returned it, not a card refund), its
     reason is "Payment reversed in a dispute", and Hardcount emails it to
     no one. The dispute's `credit_note_id` names it.
   - **It takes back the unused credit** that money paid for, at the price
     paid. A note that uses up what the credit notes allow takes all the
     unused credit. A dispute lost again after an appeal writes no second
     note.
   - **The notice is a warning with the figures:** what the bank returned,
     the credit taken back, and earlier refunds on the payment. It is
     critical while the note is not written yet. That happens when
     Hardcount could not read the customer's usage not invoiced yet as the
     loss arrived (the log line `lost dispute on a top-up recorded without
     its credit note`). Write it then:

     ```bash
     curl -s -X POST "$API/v1/invoices/$INVOICE_ID/disputes/$DISPUTE_ID/record-loss" \
       -H "Authorization: Bearer $KEY" -H "Idempotency-Key: record-loss-$DISPUTE_ID"
     ```

     **You see:** the dispute, with `credit_note_id` set. `$DISPUTE_ID` is
     the dispute's `id` (`hc_dsp_…`) from the disputes list. A `409` says
     why it does not apply: the dispute is not lost, its loss is already
     recorded, it is in another currency, or the credit notes already
     return all of the payment.
   - **A dispute in another currency** writes no note, and the notice says
     so. If the customer is not owed that credit, refund the unused credit
     with commit relief, its money as `out_of_band_amount_cents`.
   - **After a partial loss,** relief can still refund what the credit notes
     leave, but nothing more can go back to the card: relief to the card
     answers `422` and says to send the money as `out_of_band_amount_cents`
     if you pay it back another way. Hardcount does not pick that for you,
     because it records no payment nobody stated.

   **If the invoice also shows "Payment needs review"** (for example, a
   second payment), the notice names the payment the bank took back. Check
   both payments in Stripe before you refund anything: a refund of the other
   payment can give the customer the same money back twice.
4. **If it is `prevented`, check the payment in Stripe.** Stripe's dispute
   prevention settles a dispute by refunding the payment. Hardcount records
   that refund as it records any refund made in Stripe: as a credit note when
   it fits the invoice, or with a notice on the invoice when it does not
   ([A refund made in Stripe](refund-made-in-stripe.md)). An inquiry closed
   by a full refund is recorded the same way.
5. **If a lost dispute later turns into a win** (Stripe calls it a late
   win), Stripe returns the money to your balance, the disputes list shows
   `won` and the notice goes away. On a top-up, the credit the loss took
   back stays taken back: grant the customer that credit again
   (`POST /v1/credits/grant`) if they keep it. If you already acted on the
   loss, undo it: a credit note you issued for the loss is final, so record
   the money again on a new invoice. Create a manual invoice for the amount
   (`POST /v1/invoices` with its line), finalize it
   (`POST /v1/invoices/{id}/finalize`), then mark it paid outside Hardcount
   (`POST /v1/invoices/{id}/record-payment`). If you billed the customer
   again and they paid, they have now paid twice: refund that invoice with
   a credit note.

## Verify

The disputes list shows the final status. For a lost dispute, the invoice
has the credit note or the new invoice you chose; on a top-up, the dispute's
`credit_note_id` names the note Hardcount wrote. The lost-dispute notice
stays on the invoice as a record that the dispute was lost, even after you
act. Only a late win removes it.

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
the [webhook events](../../api/webhook-events.md#paymentdispute_opened).
