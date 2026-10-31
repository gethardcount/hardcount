# Subscriptions

A subscription puts a customer on one or more plans and bills them period
after period. This page covers its statuses, what changes them, how
billing periods are dated, and what happens when the plan changes, the
subscription is canceled, or collection is paused.

A subscription holds items. Each item is a plan and a quantity, such as
3 seats of "Team". The quantity multiplies the plan's base fee. Usage is
billed by the plan's prices, whatever the quantity.

## Statuses

| Status | What it means | What you do |
|---|---|---|
| `draft` | Set up, but not started. Bills nothing. | Start it with `POST /v1/subscriptions/{id}/activate`. |
| `trialing` | In a free trial. The trial bills nothing. | Nothing. It becomes `active` when the trial ends. |
| `active` | Billing, period after period. | Nothing. |
| `canceled` | Ended. It never bills again, and it cannot be restarted. | Create a new subscription to bill the customer again. |

Paused collection is not a status. A subscription whose collection is
paused stays `active` and keeps making invoices; see
[Pause collection](#pause-collection).

## What changes a subscription

All the routes are under `/v1/subscriptions`. Each change sends a
[webhook](../webhooks.md#subscriptions-datasubscription).

| From | Caused by | Webhook | To |
|---|---|---|---|
| (new) | `POST /` | `subscription.created` | `active` |
| (new) | `POST /` with `trial_days` | `subscription.created` | `trialing` |
| (new) | `POST /` with `"status": "draft"` | `subscription.created` | `draft` |
| `draft` | `POST /{id}/activate` | `subscription.activated` | `active` |
| `trialing` | Hardcount, when `trial_end_at` passes | `subscription.trial_ended` | `active` |
| `trialing` | `POST /{id}/end-trial` | `subscription.trial_ended` | `active` |
| `trialing` | `POST /{id}/extend-trial`, with a later `trial_end` | `subscription.trial_extended` | `trialing` |
| `draft`, `trialing`, `active` | `POST /{id}/cancel` | `subscription.canceled` | `canceled` |
| `trialing`, `active` | `POST /{id}/schedule-cancel` | `subscription.cancel_scheduled` | no change yet |
| `trialing`, `active` | Hardcount, when the scheduled cancel is due | `subscription.canceled` | `canceled` |
| `trialing`, `active` | `DELETE /{id}/scheduled-cancel` | `subscription.cancel_cleared` | no change |
| `active` | Dunning's final action, when the policy says cancel | `subscription.canceled` | `canceled` |

A trial ends whether or not the customer has saved a card. The first
invoice is then collected like any other. A customer with no card gets an
email: a link to add a card when Stripe is connected, or the invoice itself
when it is not.

## Billing periods

A billing period is the stretch of time one invoice covers. It includes its
start and not its end. Periods start and end at midnight in your account's
billing timezone (a setting, UTC unless you change it).

A plan bills monthly or yearly. `billing_time` on the subscription sets the
day monthly periods start on:

- **`calendar`, the default:** the 1st of each month. A subscription that
  starts on June 18 has a short first period, June 18 to July 1, then
  July 1 to August 1, and so on.
- **`anniversary`:** the day the subscription started. A subscription that
  starts on June 18 bills June 18 to July 18, then July 18 to August 18.

Yearly plans always bill on the anniversary. A subscription with a trial
starts its first paid period on the day the trial ends.

When the start day does not exist in a month, the period starts on that
month's last day, and goes back to the start day when it can. A
subscription that starts on January 31, on anniversary billing, bills:

| Period | Start | End |
|---|---|---|
| 1 | January 31 | February 28 (February 29 in a leap year) |
| 2 | February 28 | March 31 |
| 3 | March 31 | April 30 |
| 4 | April 30 | May 31 |

## Plan changes

Change an item's plan or quantity with
`PATCH /v1/subscriptions/{id}/items/{itemID}`. Send one of:

- `{"quantity": 5}`: applies now.
- `{"new_plan_id": "hc_pln_…"}`: applies at the end of the current period.
  Hardcount sends `subscription.pending_change.scheduled`, and
  `subscription.pending_change.applied` when it applies. Cancel it with
  `DELETE /v1/subscriptions/{id}/items/{itemID}/pending-change`.
- `{"new_plan_id": "hc_pln_…", "immediate": true}`: applies now.

Add an item with `POST /v1/subscriptions/{id}/items` and remove one with
`DELETE /v1/subscriptions/{id}/items/{itemID}`.

A change that applies now is prorated: the customer pays for each plan only
for the days it was on. How depends on when the base fee is billed:

- **Billed in arrears:** nothing happens now. The period's invoice bills
  each part of the period at its own plan, for its own days.
- **Billed in advance:** the period is already paid, so Hardcount settles
  the difference now. An upgrade makes an invoice now. A downgrade or a
  smaller quantity makes a credit note: a paid share goes to the
  customer's credit balance, and an unpaid share lowers what the open
  invoice asks for.

An upgrade on a plan billed in advance is refused with
`409 unpaid_invoice_blocks_change` while the period's invoice is unpaid.
Settle or void that invoice first.

Usage is always billed at the end of the period. Each part of the period
is billed on the plan that was in force during it.

### Worked example: an upgrade on June 11

A customer is on "Starter", $30.00 a month, billed in advance on calendar
billing. June's $30.00 invoice is paid. At 00:00 on June 11 you move them
to "Pro", $60.00 a month, with `"immediate": true`.

- June has 30 days, and 20 are left (June 11 to July 1).
- The difference is (6,000 − 3,000) cents × 20 ÷ 30 = 2,000 cents.

Hardcount makes an invoice now for $20.00, with two lines:

| Line | Sum | Amount |
|---|---|---|
| Unused time on Starter (after Jun 11, 2026) | −3,000 × 20 ÷ 30 | −$20.00 |
| Remaining time on Pro (after Jun 11, 2026) | the difference, plus the unused time | +$40.00 |

The amounts are rounded once, half to the even cent. Days are whole days.
A change at 14:00 on June 11 leaves 19 days and 10 hours, which rounds to
19 days. The invoice is then (6,000 − 3,000) × 19 ÷ 30 = 1,900 cents,
$19.00.

The same change on plans billed in arrears makes no invoice now. July 1's
invoice bills Starter for 10 days and Pro for 20:
3,000 × 10 ÷ 30 = 1,000 cents, plus 6,000 × 20 ÷ 30 = 4,000 cents, so
$50.00 of base fee.

## Cancel

- **Now:** `POST /v1/subscriptions/{id}/cancel`. Hardcount makes a final
  invoice for the part of the period so far: usage, and a base fee billed
  in arrears for those days. A base fee paid in advance is credited for the
  days left. A trial that never became active gets no final invoice.
- **Later:** `POST /v1/subscriptions/{id}/schedule-cancel`, with
  `{"at_period_end": true}` or `{"cancel_at": "2026-09-30T00:00:00Z"}`.
  Until then the subscription bills as usual. On a trial, `at_period_end`
  cancels free when the trial ends.

## Pause collection

`PUT /v1/subscriptions/{id}/pause-collection` with
`{"behavior": "keep_as_draft"}` pauses collection. The subscription keeps
its periods, and each new invoice stays a draft: not issued, not charged
and with no payment retries. Add `"resumes_at"` to end the pause at a set
time.

- `DELETE /v1/subscriptions/{id}/pause-collection` resumes it. Invoices of
  periods that ended while paused stay drafts: finalize or void each one
  yourself.
- A pause does not stop the retries of an invoice issued before it. Void
  or write off that invoice to stop them.
- Dunning can pause collection as its final action. That pause ends by
  itself when the customer pays the invoice.

`GET /v1/subscriptions?collection=paused` lists paused subscriptions.

## Before a change: the ended period is billed first

Each change above first bills any period that has already ended, then
applies the change. If that period cannot be billed, the change is refused
with `409 unbilled_period`, and nothing changes.

**Next:** the invoice a period becomes: [Invoices](invoices.md).
More: every route and field is in the [API reference](../api-surface.md).
