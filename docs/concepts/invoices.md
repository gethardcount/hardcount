# Invoices

An invoice is the bill for one customer: what they used and what they owe.
This page covers when Hardcount makes one, how it works out the total, how
an invoice moves from draft to paid, and what it sends your app at each
step.

## When Hardcount makes an invoice

Hardcount makes a subscription's invoice when a billing period ends:

1. The period ends, for example at 00:00 on July 1.
2. Hardcount waits the invoice grace period, `INVOICE_GRACE_PERIOD`, so
   usage that arrives a little late is still billed. It is 1 hour by
   default.
3. The next billing run makes the invoice. The run is hourly, or every 5
   minutes when `APP_ENV` is `local` or not set.

So by default the invoice appears 1 to 2 hours after the period ends. A
period with nothing to bill makes no invoice. On a test clock, moving the
clock past the period end makes the invoice at once, with no grace period.

Hardcount also makes an invoice:

- for a plan change that charges the difference now
  ([Subscriptions](subscriptions.md#plan-changes));
- for the part of a period before a cancel;
- when a spend threshold is crossed: an invoice made early because usage
  reached a limit you set ([Spend thresholds](#spend-thresholds-an-invoice-made-early));
- on the first day, for a plan whose base fee is billed in advance.

You make one-off invoices yourself, for example to sell prepaid credit or
to bill late usage: `POST /v1/invoices`, then add lines and finalize it.

More: [Usage events: late usage](usage-events.md#late-usage).

## Statuses

| Status | What it means |
|---|---|
| `draft` | Not issued. Nothing is charged or emailed. You can add lines to a one-off draft. A subscription's draft keeps the lines and the tax that the billing run gave it. |
| `finalized` | Issued and owed. The dashboard shows it as **Open** while money is due. |
| `paid` | Settled, by card, by credit, by a credit note (a document that lowers what the customer owes), or by a payment you recorded. |
| `voided` | Canceled, as if it never existed. |
| `uncollectible` | Written off: Hardcount stops collecting it, but keeps it in your records. |

A period's invoice is made already `finalized`. It stays a `draft` in two
cases: tax could not be worked out yet, or collection is paused on the
subscription. A one-off invoice starts as a `draft`.

Beside its status, an invoice has a `payment_status`: where its card
payment stands. See [Payments](payments.md#payment-statuses).

## How an invoice changes

Each route is under `/v1/invoices/{id}`.

| From | Route | To | Webhook |
|---|---|---|---|
| `draft` | `POST …/finalize` | `finalized` | `invoice.finalized` |
| `finalized` | Hardcount collects it: credit, then the card | `paid` | `invoice.paid`; `payment.succeeded` for a card payment |
| `finalized`, `uncollectible` | `POST …/record-payment`, for a payment made outside Hardcount | `paid` | `invoice.payment_recorded` and `invoice.paid` |
| `draft`, `finalized`, `uncollectible` | `POST …/void` | `voided` | `invoice.voided` |
| `finalized` | `POST …/mark-uncollectible` | `uncollectible` | `invoice.marked_uncollectible` |

- A `paid` invoice cannot be voided. Issue a credit note instead.
- While a card payment may still go through, a void, a write-off or a
  recorded payment is refused with a `409`, usually
  `payment_in_flight`. Wait for the payment to settle
  ([Payments](payments.md#when-the-answer-is-unclear)). A parked invoice
  can still be written off after a few minutes.
- A void gives back any credit the invoice used, to the credit grants it
  came from ([Credits](credits.md#credit-given-back)).
- A draft sends no webhook. A period's invoice that is made already
  finalized sends `invoice.finalized` once, like a draft you finalize.

## How the total is worked out

Hardcount works out an invoice in this order:

1. **Usage.** Each meter adds up the period's events. Each price takes its
   events ([Meters and prices](meters-and-prices.md)).
2. **Lines.** Each price with usage becomes a line, and the base fee is a
   line of its own. Each line is rounded to whole cents once, half to the
   even cent. A price with no usage makes no line.
3. **Subtotal.** The lines added together, in `subtotal_cents`.
4. **Tax.** Worked out on the subtotal by your tax setup, in
   `tax_amount_cents` ([Tax](../ops/tax-calculation.md)).
5. **Total.** Subtotal plus tax, in `total_amount_cents`. At first,
   `amount_due_cents` is the same.

With tax-inclusive pricing (**Settings → Tax**), your prices already hold
the tax. Hardcount then takes the tax out of each line instead of adding
it on top. So the subtotal is lower than the sum of the line prices, and
the total equals that sum.

Then, right after the invoice is finalized, Hardcount collects it:

6. **Credit.** Any credit the customer holds in the invoice's currency
   lowers `amount_due_cents`, and `credits_applied_cents` shows by how
   much. The total does not change, and credit pays the tax too.
7. **Card.** What is still due is charged to the customer's saved card
   ([Payments](payments.md)).

Because credit comes after finalizing, the `invoice.finalized` webhook
shows the amount due before credit. `invoice.paid` shows the final split.

### Worked example

A June invoice has a $20.00 base fee and three usage lines of $3.00, $2.62
and $0.60. The account uses a fixed 10% tax rate. The customer holds $10.00
of prepaid credit in USD.

| Field | Sum | Value |
|---|---|---|
| `subtotal_cents` | 2,000 + 300 + 262 + 60 | 2,622 |
| `tax_amount_cents` | 10% of 2,622 = 262.2, rounded | 262 |
| `total_amount_cents` | 2,622 + 262 | 2,884 |
| `credits_applied_cents` | the $10.00 of credit | 1,000 |
| `amount_due_cents` | 2,884 − 1,000 | 1,884 |

The card is charged $18.84. When it succeeds, `amount_paid_cents` is 1,884
and `amount_due_cents` is 0. The total stays $28.84.

## Due date and number

- **Due date.** `due_at` is the issue date plus your payment terms: 30 days
  unless you change the setting, from 0 (due on receipt) to 365.
- **Number.** Each invoice gets the next number in its mode, such as
  `HC-000001`. Test and live mode count apart. The prefix is a setting.

## PDF and the hosted invoice page

- **PDF:** `GET /v1/invoices/{id}/pdf`, or **⋯** then **Download PDF** on
  the invoice page in the dashboard.
- **Hosted invoice page:** a public page for one issued invoice, which the
  customer opens without signing in. Its address is
  `HOSTED_INVOICE_BASE_URL` + `/invoice/` + the invoice's `public_token`.
  A draft has no page. Invoice emails link to it. The page has a **Pay**
  button, which opens Stripe Checkout, when all of these hold:
  - the invoice is `finalized`, so not paid, voided or written off;
  - money is due;
  - no payment is in flight;
  - Stripe is connected in the invoice's mode;
  - the server has `HOSTED_INVOICE_BASE_URL`, the page's own address, for
    Stripe to send the customer back to;
  - Stripe has not refused the amount as below its minimum charge
    ([what to do](../ops/runbook/below-stripe-minimum.md)).

## Spend thresholds: an invoice made early

A spend threshold makes Hardcount invoice a subscription's usage before
its period ends. It bills sooner; it never stops usage. Set it with
`PUT /v1/subscriptions/{id}/billing-thresholds`:

- `amount_gte`: an amount in cents. It is crossed when the period's usage
  so far reaches it.
- `item_thresholds`: a list of `{subscription_item_id, usage_gte}`. One is
  crossed when that item's usage so far reaches `usage_gte`.

When one is crossed, Hardcount issues an invoice for the usage so far,
with `billing_reason` `threshold` and no base fee, and sends
`subscription.threshold_crossed`. The period keeps its dates, and the
period's own invoice bills the rest. A threshold fires at most once a
period. Usage of a `max` or `last` meter waits for the period's own
invoice. `DELETE /v1/subscriptions/{id}/billing-thresholds` removes them.

What happens to the usage a spend-threshold invoice billed depends on what
you do with that invoice, and when:

- **Write it off** (mark it uncollectible): the usage stays billed on it.
  The period's own invoice does not bill it again. If the customer pays the
  written-off invoice later, it simply reads paid.
- **Void it before the period's own invoice is built:** that invoice bills
  the usage instead, from the period start. Do not also bill it on a
  one-off invoice.
- **Void it after the period's own invoice is built:** the usage is billed
  nowhere, because that invoice does not change. To charge it, bill it on a
  one-off invoice
  ([Usage is missing from a built invoice](../ops/runbook/invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice),
  step 3). A void that arrives while that invoice is being saved waits for
  it, and then counts as after.

A spend-threshold invoice made before version 0.9.0 can also carry the base
fee of a plan billed in arrears. These rules apply to that fee as to the
usage.

Usage of a `sum` or `count` meter
([aggregation](meters-and-prices.md#meters)) that is stamped before a
spend-threshold invoice but arrives after it is on neither invoice. While
the spend-threshold invoice can still be voided, such usage is not flagged
`after_close` ([late usage](usage-events.md#late-usage)): a void would give
its window back. A written-off invoice can still be voided, so usage behind
it is not flagged either. Such usage that arrives once that invoice is paid
is flagged. The
[late-usage steps](../ops/runbook/invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice)
find it either way.

## Credit notes

A credit note lowers what a customer owes on an invoice. Create one with
`POST /v1/credit-notes` on a `finalized` or `paid` invoice. It is a draft
until you issue it with `POST /v1/credit-notes/{id}/issue`, or at once with
`"auto_issue": true`. Only a draft credit note can be voided.

- **On an unpaid invoice,** it lowers `amount_due_cents`. If nothing is
  left due, the invoice is marked `paid`. If the credit note is for more
  than what is still due, the extra part gives back credit that the invoice
  used. That credit returns to the credit grants it came from
  ([Credits](credits.md#credit-given-back)).
- **On a paid invoice,** its total is split between a refund to the card,
  credit to the customer's balance, and money you returned outside
  Hardcount. With no split given, all of it goes to the credit balance.

## Webhooks

| Event | When |
|---|---|
| `invoice.finalized` | The invoice is issued. It shows the amount before credit. |
| `invoice.paid` | Nothing is left due, whatever paid it. |
| `invoice.payment_recorded` | You recorded a payment made outside Hardcount. |
| `invoice.voided` | The invoice is voided. |
| `invoice.marked_uncollectible` | The invoice is written off. |
| `payment.succeeded`, `payment.failed` | A card charge succeeded or failed ([Payments](payments.md#webhooks)). |

More: every event, with an example message: [Webhook events](../api/webhook-events.md#invoices-datainvoice).

**Next:** how prepaid credit lowers an invoice: [Credits](credits.md).
