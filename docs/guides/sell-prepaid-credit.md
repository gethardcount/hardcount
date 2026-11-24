# Sell prepaid credit

This guide sells a customer prepaid credit with the API: they pay up front,
and later invoices in the same currency draw the credit down. It walks
through the five calls, what the customer sees, and how to top up a
customer's credit automatically from your own code.

Think of a gift card: the customer buys it once, and each purchase after
that spends from it.

## Before you start

- Hardcount running, with its address in `HARDCOUNT` and a test secret key
  in `HARDCOUNT_KEY` ([first invoice](../first-invoice.md#before-you-start)).
- A customer with an email address, with its Hardcount ID (`hc_cus_…`) in
  `CUSTOMER_ID`.
- To charge the customer's saved card, or to let them pay by card on their
  payment page: Stripe connected in this mode
  ([Connect Stripe](connect-stripe.md)). Recording a payment you received
  needs neither.

## Words on this page

| Word | Meaning |
|---|---|
| Top-up | The invoice that sells the credit: `billing_reason` `topup`, with one line. It is a draft with no number until it is paid. |
| Commit credit | The credit a paid top-up adds to the customer's balance (`grant_kind` `commit`). |
| Withdraw | Void a top-up nobody paid. It granted nothing, and its link stops working. |

In the dashboard, **Sell credit** on the customer's Credits section, or on
the Credits page, makes the same top-up as step 1.

## How long the credit lasts

Credit bought with a top-up stays usable for `topup_credit_valid_days` after
its payment. Set it in the dashboard: **Settings → Invoicing → Bought credit
lasts (days)**. Leave it blank and the credit never expires, which is the
default.

A top-up copies the setting when it is created, so a change applies to
top-ups created after it. The setting is changed from a dashboard session:
`PUT /v1/settings` with an API key answers `403` "this endpoint requires a
dashboard session".

## 1. Create the top-up

An invoice with one line that carries `commit_granted_cents`, and nothing
else, is a top-up. `unit_amount_cents` is what the customer pays, and
`commit_granted_cents` is the credit they get. Here they pay $90 for $100 of
credit.

```bash
TOPUP=$(jq -n --arg cus "$CUSTOMER_ID" \
    '{customer_id: $cus, line_items: [{description: "Prepaid credit",
      line_type: "add_on", quantity: 1, unit_amount_cents: 9000,
      commit_granted_cents: 10000}]}' \
  | curl -s -X POST "$HARDCOUNT/v1/invoices" \
    -H "Authorization: Bearer $HARDCOUNT_KEY" \
    -H "Content-Type: application/json" \
    -d @-)
TOPUP_ID=$(echo "$TOPUP" | jq -r .id)
echo "$TOPUP" | jq -c '{id, status, billing_reason, invoice_number, amount_due_cents, due_at, tax_amount_cents}'
```

**You see:**
`{"id":"hc_inv_dc2q1ejmajdrmuv09q60","status":"draft","billing_reason":"topup","invoice_number":"","amount_due_cents":9000,"due_at":null,"tax_amount_cents":0}`

The top-up has no number and no due date: nothing is owed until it is paid.
The purchase carries no tax, because the usage the credit pays for is taxed
([tax on prepaid credit](../ai-pricing.md#tax-on-prepaid-credit)). Its line,
read with `GET /v1/invoices/$TOPUP_ID`, shows `credit_valid_days` when the
credit expires, and leaves it out when it never does.

A top-up is never finalized: its payment issues it. Finalizing it is
refused.

```bash
curl -s -X POST "$HARDCOUNT/v1/invoices/$TOPUP_ID/finalize" \
  -H "Authorization: Bearer $HARDCOUNT_KEY"
```

**You see:** `409` with code `invalid_state` and the message "A top-up is
issued when it is paid: collect it, send it, or record a payment."

## 2. Take the payment

Pick one of the three ways. Each one, when the payment arrives, numbers the
top-up, issues it, marks it paid and adds the credit, in one step.

### Charge the saved card

Send an `Idempotency-Key`. A retry with the same key answers what the first
call answered and never charges twice
([idempotency](../api/idempotency.md)).

```bash
curl -s -X POST "$HARDCOUNT/v1/invoices/$TOPUP_ID/collect" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Idempotency-Key: $(uuidgen)"
```

**You see:** the invoice, with `status` `paid`, its new `invoice_number` and
`payment_status` `succeeded`. Other answers:

- **No saved card:** `422` `validation_error`, "customer has no payment
  method set up". Nothing was charged. Send the link instead.
- **The card is declined:** `502` `provider_error`, with the reason. The
  top-up stays an unpaid draft, and the customer gets no email. A retry with
  the same key answers the same `502`; to charge again, use a new key.
- **Stripe still confirming:** `200` with `payment_status` `processing`. The
  `invoice.paid` webhook event follows when the payment settles.

### Send the payment link

The customer gets an email with a link to their payment page, where they pay
by card.

```bash
curl -s -X POST "$HARDCOUNT/v1/invoices/$TOPUP_ID/send" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"email": "ap@globex.test"}'
```

**You see:** the email queued. The send is refused with `409` when the link
could not work, and the message says why. Without Stripe connected in this
mode, it begins "This top-up's email is a link to pay it by card, and Stripe
isn't connected in this mode".

### Record a payment you received

For a bank transfer, a cheque or cash. `note` is your reference.

```bash
curl -s -X POST "$HARDCOUNT/v1/invoices/$TOPUP_ID/record-payment" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"note": "Wire 1042"}' \
  | jq -c '{status, invoice_number, amount_paid_cents, paid_out_of_band, due_at}'
```

**You see:**
`{"status":"paid","invoice_number":"HC-000001","amount_paid_cents":9000,"paid_out_of_band":true,"due_at":null}`

## 3. Check the credit

```bash
curl -s "$HARDCOUNT/v1/credits/balance/$CUSTOMER_ID" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  | jq -c '.data[] | {currency, balance_cents, total_granted}'
```

**You see:** `{"currency":"USD","balance_cents":10000,"total_granted":10000}`

The customer paid $90 and holds $100 of commit credit.

## Withdraw a top-up nobody paid

Void it. It keeps no number, granted nothing, and its link stops working.
Here `UNPAID_ID` is a second top-up, created as in step 1 and not paid.

```bash
curl -s -X POST "$HARDCOUNT/v1/invoices/$UNPAID_ID/void" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  | jq -c '{status, invoice_number}'
```

**You see:** `{"status":"voided","invoice_number":""}`

A charge on it afterwards is refused with `422`. A card payment that still
arrives is recorded as a payment on a voided invoice, which raises the
`payment.received_on_voided_invoice` event and grants no credit.

## What the customer sees

**The email** (step 2, send): the subject "Buy prepaid credit from Acme
Cloud", then:

> Acme Cloud sent you a link to buy USD 100.00 of prepaid credit for USD 90.00.
>
> The credit is added to your account as soon as your payment goes through.
> It is valid for 365 days after payment.
>
> This is not a bill: you pay only if you want the credit.

and a **Buy credit** button. The validity sentence is "It never expires."
when no validity is set.

**The payment page**, in each state:

| State | What it shows |
|---|---|
| Unpaid | "Buy prepaid credit", then "You pay $90.00. You get $100.00 of credit, added to your account as soon as your payment goes through.", the validity sentence and a **Pay $90.00** button. No number, no amount due, no PDF. |
| Paid | The invoice, with its number and "Paid", then "Credit added: $100.00", the validity sentence and its PDF. |
| Withdrawn | "Offer withdrawn", then "This offer was withdrawn by Acme Cloud." No number and no PDF. |

**The receipt**, once paid: the usual receipt for the new invoice, plus
"Credit added: USD 100.00" and the validity sentence.

**No PDF until it is paid.** `GET /v1/invoices/{id}/pdf`, and the PDF link
of the payment page, answer `409` `invalid_state`, "A top-up has no document
until it is paid.", for an unpaid or withdrawn top-up. A paid top-up's PDF is
its invoice.

## Its history

`GET /v1/invoices/$TOPUP_ID/payment-timeline` answers for an unpaid top-up
too: its creation, its declined charges and the emails sent for it. The
dashboard shows the same rows on the top-up's page.

```bash
curl -s "$HARDCOUNT/v1/invoices/$TOPUP_ID/payment-timeline" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" | jq -c '.events[] | .line.title'
```

**You see**, for the top-up paid by bank transfer above: `"Top-up created"`,
`"Invoice finalized"`, `"Offline payment received"`. A withdrawn one ends in
`"Top-up withdrawn"`.

## Automatic top-up with your own code

Hardcount does not top up a customer's credit by itself yet. Your code can:
when the customer's credit runs low, create a top-up and charge their saved
card.

**The event:** `credit.balance_low`
([its message](../api/webhook-events.md#creditbalance_low)). Its
`data.credit_balance` names the customer (`customer_id`), the `currency` and
the `balance_cents` after the change, and `data.threshold_cents` is your
threshold.

- **The threshold** is **Settings → Invoicing → Credit low-balance alert**
  (`credit_balance_low_threshold_cents`). It is one number for the whole
  account, applied in each currency's own units, not one per customer.
  With none set, the event is never sent.
- **It fires once per crossing:** when one change takes a currency's balance
  from at or above the threshold to below it. Further spending below the
  threshold sends nothing more, until the balance is back at or above it.
- **It follows the settled balance**, which changes when an invoice, a
  grant, an adjustment or an expiry is written, not as usage arrives
  ([why](../webhooks.md#the-credit-events-follow-the-settled-balance)).
- `credit.balance_depleted` fires when a balance reaches zero or less, with
  or without a threshold. Handle it the same way if you top up only when
  the credit is gone.

**On each `credit.balance_low` event:**

1. **Skip an event you have handled.** Store each event's `id` (`hc_whevt_…`)
   and stop if you have seen it
   ([handle each event once](receive-webhooks.md#handle-each-event-once)).
   Hardcount retries a delivery for about 26 hours, and an idempotency key
   lasts 24, so your own record is what stops a late retry.
2. **Read the balance again** with `GET /v1/credits/balance/{customer_id}`.
   If it is back at or above your threshold, a payment already landed: stop.
3. **Reuse an unpaid top-up.** List the customer's drafts and keep a top-up
   in the event's currency:

   ```bash
   curl -s "$HARDCOUNT/v1/invoices?customer_id=$CUSTOMER_ID&status=draft" \
     -H "Authorization: Bearer $HARDCOUNT_KEY" \
     | jq -c '[.data[] | select(.billing_reason == "topup") | {id, currency, amount_due_cents}]'
   ```

   **You see:** `[{"id":"hc_inv_dc2q1ejmajdrmuv09q60","currency":"USD","amount_due_cents":9000}]`,
   or `[]` when there is none. Never create a new top-up for each webhook.
4. **Otherwise create one** as in step 1, with the header
   `Idempotency-Key: topup-create-<event id>`.
5. **Charge it** with `POST /v1/invoices/{id}/collect` and the header
   `Idempotency-Key: topup-collect-<event id>`. The event id is random and
   unique, so a key built from it is too. A redelivered event within 24 hours
   gets the first answer back and charges nothing.

**What the charge answers, and what to do:**

| Answer | What happened | Do |
|---|---|---|
| `200`, `status` `paid` | Paid, and the credit added | Nothing |
| `200`, `payment_status` `processing` | Stripe is confirming the payment | Wait for `invoice.paid` |
| `422`, "customer has no payment method set up" | No saved card. Nothing charged | Send the link (step 2) |
| `502` `provider_error` | Declined. The top-up stays an unpaid draft | Leave it for the next event, which reuses it, or withdraw it |
| `502` `payment_outcome_unknown` | Stripe never answered; the card may be charged | Do not retry. Hardcount checks with Stripe and updates the top-up; watch for `invoice.paid` |
| `409` `payment_in_flight` | A charge on it is still running | Nothing |

## What to ask a lawyer

Prepaid credit can fall under consumer and payment rules that differ by
country and state. Hardcount gives no legal advice. Before you sell credit,
ask a lawyer where you sell:

- **Credit that expires:** what notice of the expiry you must give, and
  when. Whether consumer law limits expiry on bought credit, or sets a
  shortest allowed period. Whether unclaimed-property, escheat or gift-card
  rules apply to credit that is not spent.
- **Refunds:** whether a customer may claim back unspent credit, and on
  what terms. Taking credit back with a credit note is your decision in
  Hardcount, for each top-up.
- **What your terms and your invoices must say** about the credit, its
  validity and refunds.

**Next:** gate usage on what the customer can still spend:
[Stop usage when credit runs out](../stop-usage-when-credit-runs-out.md).
