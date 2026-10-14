# Stop usage when credit runs out

This page is for a developer whose customers prepay for credit and then use
it up. It shows how to find out, from your own code, that a customer has run
out, so that you can stop serving them.

The short version:

1. Ask Velox for the customer's live credit balance on an interval.
2. Keep the answer in your own cache.
3. Check your cache before you serve a request.

## Velox records usage. It never refuses it.

Velox accepts every usage event you send, including for a customer who has
no credit left. It does not block, throttle or reject usage. The decision to
stop serving a customer is always yours, and it runs in your code.

This is deliberate. A billing system that goes down, or answers slowly,
should not take your product down with it.

## The read

```
GET /v1/credits/balance/{customer_id}/live
Authorization: Bearer <secret key>
```

```json
{
  "customer_id": "vlx_cus_…",
  "balance_cents": 4000,
  "unbilled_usage_cents": 1000,
  "estimated_remaining_cents": 3000,
  "computed_at": "2026-10-13T09:30:00Z"
}
```

| Field | Meaning |
|---|---|
| `balance_cents` | The credit the customer can spend now: what is left after every invoice issued so far. A grant that is past its expiry date is not counted. |
| `unbilled_usage_cents` | What the usage of the current period will cost when it is invoiced. |
| `estimated_remaining_cents` | The first minus the second, never below zero. Gate on this. |
| `computed_at` | When Velox worked the figures out. |

A customer id that is not in your tenant and mode answers `404`.

`estimated_remaining_cents` stops at zero. To see how far past zero a
customer has gone, subtract `unbilled_usage_cents` from `balance_cents`
yourself.

## Poll it and cache it. Do not call it on every request.

Each call adds up the customer's usage events for the current billing
period. The more events the customer has sent this period, the longer the
call takes. On one laptop running Postgres 16, a customer with 100,000 events
in the period took about 0.2 seconds. A customer with 1,000,000 events took
between 1.7 and 2.5 seconds. Measure it on your own hardware before you
choose a poll interval.

So treat it as a background read:

- Poll once a minute for each customer who has sent usage since your last
  poll.
- Keep polling each customer you are refusing, too. A refused customer sends
  no usage, so a poll that follows usage alone would never look at them
  again.
- Store `estimated_remaining_cents` in your own cache (Redis, memory, your
  database), with no expiry time.
- Before you serve a request, read your cache. Never call Velox in the
  request path.

A stored zero must stay until a later read replaces it. If the entry expires,
or your cache drops it, a customer you were refusing is served again. So
write the entry only after a read that worked, and never let a missing entry
turn a refused customer back on.

Between two polls a customer can spend a little more than they have. With a
one-minute poll that is at most one minute of usage. Poll faster for a
customer who is close to zero if that matters to you, or stop serving a
little above zero.

```python
# Runs on a timer, once a minute.
for customer_id in customers_active_since_last_poll() | customers_refused():
    try:
        live = velox.get(f"/v1/credits/balance/{customer_id}/live", timeout=30)
    except Exception as err:
        alert(customer_id, err)   # keep the last figure, go on to the next customer
        continue
    cache.set(f"credit:{customer_id}", live["estimated_remaining_cents"])   # no expiry

# Runs in your request path.
def may_serve(customer_id):
    remaining = cache.get(f"credit:{customer_id}")
    if remaining is None:
        return True   # never polled yet. Your choice: serve or refuse
    return remaining > 0
```

`customers_refused()` is every customer whose stored figure is zero.

## When the read fails

The read answers `500` when Velox cannot work the figure out, and it gives up
after 30 seconds. It never returns part of a figure.

- Keep the last figure you stored for that customer. Do not clear it.
- Go on to the next customer. One failed read must not stop the loop.
- Alert yourself. If a customer's pricing is set up wrongly, the read fails
  for that customer every time until the pricing is fixed.

A customer with tens of millions of usage events in one billing period may
not get an answer inside the 30 seconds. Measure your largest customers
first.

## A top-up shows at once

Velox stores nothing for this read. It works the figure out again on every
call. When you grant credit, the next read already includes it, so poll that
customer again straight after a top-up and update your cache.

## What the figure includes

- Every credit grant, adjustment and expiry.
- Every invoice that has already drawn on the credit.
- The metered usage of the current period that no invoice has billed yet,
  priced with the same rules the invoice will use.
- Each subscription counted over its own billing period, when a customer has
  more than one.

Four cases are handled so that nothing is counted twice or wrongly:

- **A billing threshold invoiced part of the period early.** That usage has
  been paid for from credit already, so only the usage since that invoice is
  counted.
- **A subscription is in a trial.** Usage sent before the first paid period
  starts is not billed, so it is not counted. The first paid period starts at
  the beginning of the day the trial ends. Usage sent on that last day is
  billed, so it is counted, even while the subscription still says
  `trialing`. A trial that is set to cancel when it ends is not billed at
  all, and none of its usage is counted.
- **The plan changed in the middle of the period.** Each meter is counted for
  the time a plan that carries it was on the subscription, as the invoice
  bills it. Usage on the old plan's meter before the change still counts.
- **A grant has passed its expiry date.** It is no longer counted, even if
  the hourly run has not yet written the expiry to the ledger. During that
  hour `balance_cents` here is lower than the balance
  `GET /v1/credits/balance/{customer_id}` returns.

## What the figure leaves out

The figure is an estimate. It can read higher than what the customer really
has left, because these are not subtracted:

- **Tax.** The invoice adds tax; the estimate does not.
- **A base fee billed at the end of the period.** It appears only when the
  period closes and the invoice is issued.
- **Unpaid invoices.** An invoice that is issued but not yet settled can
  still draw on the credit.
- **Usage sent after a period has ended and before Velox has closed it.**
  Velox closes a period on the first hourly billing run after
  `INVOICE_GRACE_PERIOD` has passed, so this gap lasts 1 to 2 hours by
  default ([ADR-120](adr/120-invoice-grace-period-for-late-usage.md)).
- **Credit that expires before the period ends.** It counts as spendable
  until its expiry date. If the period's invoice is issued after that date,
  the invoice cannot draw on it.

It can read lower than the truth in one case: a subscription with a usage
cap that blocks overage. The invoice applies the cap and the estimate does
not.

If a customer is billed in more than one currency, the amounts are added
together as plain cents.

Usage sent for a customer who has no active subscription is recorded but not
priced, so it does not lower the figure.

If these gaps matter for your product, stop serving a little above zero
rather than at zero.

## Do not use the credit webhooks for this

Velox sends three webhook events about credit: `credit.balance_low`,
`credit.balance_depleted` and `credit.balance_recovered`. They follow
`balance_cents`, the settled balance. That balance moves when an invoice,
grant, adjustment or expiry is written. It does not move when usage arrives.

A pay-as-you-go customer is invoiced at the end of the period. For that
customer, `credit.balance_depleted` fires when the invoice is issued, which
can be weeks after the credit was really used up. Use those events to send a
top-up reminder or to reconcile. Use the live read to decide whether to
serve. See [Webhooks](webhooks.md#event-catalog).

## The same figure on the public cost page

The public cost dashboard, `GET /v1/public/cost-dashboard/{token}`, returns
the same three numbers in its `credits` object. Your customer sees the
figure your backend gates on.
