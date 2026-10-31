# Payments

Hardcount charges a customer's saved card through your own Stripe account,
right after an invoice is issued. When a charge fails, it tries again on a
schedule and emails the customer. This page explains that flow, the
payment statuses, and the webhooks your app gets.

Hardcount uses Stripe to save, charge and refund cards, and, if you choose,
to work out tax. It does not use Stripe Billing or Stripe Invoices: the
invoice is Hardcount's. To connect Stripe, see
[Connect Stripe and take card payments](../guides/connect-stripe.md).

## The flow

1. **The customer saves a card.** They enter it on a Stripe page, never on
   yours or Hardcount's. That page opens from a setup link you send, or
   from **Pay** on the hosted invoice page. A card saved through a setup
   link becomes their default.
2. **An invoice is issued.** Hardcount first uses any credit the customer
   holds ([Credits](credits.md)). If credit covers it all, the invoice is
   paid and nothing is charged.
3. **Hardcount charges the default card** for what is left, naming that
   exact card. The charge is a Stripe PaymentIntent: Stripe's record of one
   attempt to take money.
4. **The charge succeeds or fails.** Success marks the invoice `paid`. A
   failure starts payment retries, called dunning.
5. **Dunning retries the charge** on a schedule and, once email is set
   up, emails the customer. A card the customer adds meanwhile is charged.
   When the retries run out, your dunning policy decides what happens.

With no card saved, step 3 cannot run. Dunning starts at once, and the
customer gets one email: a link to add a card when Stripe is connected, or
the invoice itself when it is not. Once they add a card, the next billing
run charges it.

An amount below Stripe's minimum charge, such as $0.50 in USD, is not
charged and starts no retries. The invoice stays open
([what to do](../ops/runbook/below-stripe-minimum.md)).

## Payment statuses

An invoice's `payment_status` says where its card payment stands. It sits
beside the invoice's own `status`.

| `payment_status` | What it means |
|---|---|
| `pending` | Not charged yet: the invoice is waiting for a card, a retry, or its first charge. |
| `processing` | Stripe accepted the charge but has not finished it, as with some bank debits. |
| `succeeded` | The money arrived. The invoice is `paid`. |
| `failed` | Stripe declined the charge. Dunning retries it. |
| `unknown` | Hardcount did not get a clear answer, so it cannot tell if money moved. See [When the answer is unclear](#when-the-answer-is-unclear). |

## Retries: dunning

Dunning is the process of collecting an unpaid invoice. A dunning policy
sets it up:

- **When to retry:** the days to wait before each retry. Every new account
  starts with the policy "Default retries", which retries on day 3, 7 and
  14 after the failed charge.
- **What to do when the retries run out:** pause collection, cancel the
  subscription, write off the invoice, both, or neither. "Default retries"
  does neither: the subscription keeps billing and the invoice stays open.

Each retry first uses any credit the customer got since the last try.

Some declines are never retried on the same card:

- **A card refused for good**, such as a lost or stolen card. Retries keep
  their schedule but charge only once the customer has another default card.
- **A bank that wants the customer to confirm the payment** (3-D Secure).
  The emails ask the customer to pay on the hosted invoice page, where they
  can confirm.

You see each run on the **Dunning** page, and set policies under **Dunning
policies**.

## When the answer is unclear

A charge can end without a clear answer: a timeout, a Stripe server error
or a dropped connection. Then the payment status is `unknown`, and
Hardcount never guesses.

- **Nothing charges the invoice again** until Stripe's answer is known, so
  the customer is never charged twice. A charge hold, a 5-minute claim on
  the invoice, makes sure only one charge runs at a time. After an unclear
  answer it stays in place.
- **The reconciler finds the answer.** It is a check that asks Stripe what
  happened to payments Hardcount did not hear back about. It runs with
  every billing run: hourly, or every 5 minutes when `APP_ENV` is `local`
  or not set. A Stripe webhook can also settle the payment sooner.
- **A parked invoice** is one whose unclear charge left no PaymentIntent
  ID, so Hardcount cannot look it up directly. The reconciler looks through
  the customer's payments in Stripe. If no payment appears within 24 hours
  of the charge, the invoice is charged again as usual.

While a payment is `processing` or `unknown`, Hardcount refuses to void the
invoice or to record a payment on it, so money cannot arrive on an invoice
that no longer asks for it.

More: [Card payment failure handling](../payments.md), every case in
detail.

## Webhooks

| Event | When |
|---|---|
| `payment.succeeded` | A card charge succeeded. `invoice.paid` comes with it. |
| `payment.failed` | Stripe declined a charge. |
| `dunning.started` | Dunning began on an unpaid invoice. |
| `dunning.escalated` | Every retry failed, and the policy's final action was taken. |
| `dunning.resolved` | Dunning ended, for example because the customer paid. |
| `payment.dispute_opened`, `payment.dispute_closed` | A customer disputed a card payment with their bank, and when Stripe closed the dispute. |
| `payment.refunded_in_stripe` | A refund was made in Stripe outside Hardcount. |
| `payment.duplicate_charge`, `payment.amount_mismatch`, `payment.currency_mismatch`, `payment.received_on_voided_invoice` | Money arrived that does not fit the invoice. Check it in Stripe. |

More: payloads in the [webhook event catalog](../webhooks.md#event-catalog).

**Next:** set it up: [Connect Stripe and take card payments](../guides/connect-stripe.md).
