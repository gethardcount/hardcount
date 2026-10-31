# Stop usage when credit runs out

This page is for customers who pay in advance with prepaid credit. It
shows how your app can stop serving one of them when the credit is used
up. The short version:

1. Ask Hardcount for the customer's live credit balance on an interval.
2. Keep the answer in your own cache.
3. Check your cache before you serve a request.

## Hardcount records usage. It never refuses it.

Hardcount accepts every usage event you send, including for a customer who has
no credit left. It does not block, throttle or reject usage. The decision to
stop serving a customer is always yours, and it runs in your code.

This is deliberate. A billing system that goes down, or answers slowly,
should not take your product down with it.

## Which customers to gate

Gate only the customers you sell prepaid credit to (a prepaid commit or a
credit grant). A pay-as-you-go customer, billed at the end of each period,
has no credit, so their figure is always 0: gating on it would refuse them
from their first request.

Credit pays only invoices in its own currency. So gate each customer on
the entry in the currency you bill them in. Hardcount writes currencies in
capitals, such as `USD`.

## The read

```
GET /v1/credits/balance/{customer_id}/live
Authorization: Bearer <secret key>
```

`customer_id` is Hardcount's ID for the customer (`hc_cus_…`), not your
external ID. Look it up once and keep it:

```bash
curl -s "$HARDCOUNT/v1/customers?external_id=acme-1" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" | jq -r '.data[0].id'
```

**You see:** the customer's ID, such as `hc_cus_…`.

The read needs a secret key. A usage-only key cannot read credit. It
answers:

```json
{
  "customer_id": "hc_cus_…",
  "balances": [
    {
      "currency": "USD",
      "balance_cents": 4000,
      "unbilled_usage_cents": 1000,
      "estimated_remaining_cents": 3000
    }
  ],
  "computed_at": "2026-10-13T09:30:00Z"
}
```

`balances` has one entry per currency: each currency the customer holds
credit in, and each currency a live subscription will invoice usage in.
No entry in the customer's currency means no credit in it.

| Field | Meaning |
|---|---|
| `currency` | The currency of the entry. |
| `balance_cents` | The credit the customer can spend now in this currency: what is left after every invoice issued so far. A grant that is past its expiry date is not counted. |
| `unbilled_usage_cents` | What the usage of the current period will cost when it is invoiced in this currency. |
| `estimated_remaining_cents` | The first minus the second, never below zero. Gate on this. |
| `computed_at` | When Hardcount worked the figures out. |

Here the customer has $40.00 of credit and $10.00 of usage not invoiced
yet: 4,000 − 1,000 = 3,000 cents, so $30.00 is left.

A customer ID that is not in your account and mode answers `404`.

`estimated_remaining_cents` stops at zero. To see how far past zero a
customer has gone, subtract `unbilled_usage_cents` from `balance_cents`
yourself.

## Poll it and cache it. Do not call it on every request.

Each call adds up the customer's usage for the period, so it slows as usage
grows. On one laptop: about 0.2 s at 100,000 events, 1.7 to 2.5 s at
1,000,000 ([how it was measured](../internal/billing/live_balance_timing_integration_test.go)).
The read gives up after 30 seconds. Measure your largest customers first.

So treat it as a background read:

- Poll once a minute for each prepaid customer who has sent usage since
  your last poll.
- Keep polling each customer you are refusing, too. A refused customer sends
  no usage, so a poll that follows usage alone would never look at them
  again.
- Store `estimated_remaining_cents` in your own cache (Redis, memory, your
  database), with no expiry time.
- Before you serve a request, read your cache. Never call Hardcount in the
  request path.

A stored zero must stay until a later read replaces it. If the entry expires,
or your cache drops it, a customer you were refusing is served again. So
write the entry only after a read that worked, and never let a missing entry
turn a refused customer back on.

Between two polls a customer can spend a little more than they have. With a
one-minute poll that is at most one minute of usage. Poll faster for a
customer who is close to zero if that matters to you.

### The rate limit

Each read counts toward the API's general limit: 1,000 requests a minute
for each API key. A poller that reads 1,000 customers a minute uses all of
it.

- **Give the poller its own secret key.** Each key has its own limit, so the
  poller then never slows the rest of your app's API calls.
- **On a `429`, wait the number of seconds in `Retry-After`**, then read
  that customer again. That is when the next request passes.
- **More than about 1,000 customers to poll each minute?** Poll less often,
  or raise `HARDCOUNT_RATE_LIMIT_GENERAL_PER_MIN` on your Hardcount server
  ([Settings](settings.md)).

### A poller you can run

This script uses only Python's standard library (Python 3.8 or later). It
reads each customer once a minute, keeps the last good figure, and prints
what your request path would decide.

```python
"""Poll each prepaid customer's live credit balance and keep the last good figure."""
import json
import os
import sys
import time
import urllib.error
import urllib.request

HARDCOUNT = os.environ["HARDCOUNT"]
KEY = os.environ["HARDCOUNT_POLL_KEY"]   # a key used only by this poller
CURRENCY = sys.argv[1].upper()           # the currency you bill these customers in
CUSTOMERS = sys.argv[2:]                 # Hardcount customer IDs: hc_cus_…
remaining = {}                           # your cache (Redis, memory, a table), no expiry


def read_remaining(customer_id):
    """The customer's estimated_remaining_cents, or None when the read failed."""
    req = urllib.request.Request(
        f"{HARDCOUNT}/v1/credits/balance/{customer_id}/live",
        headers={"Authorization": f"Bearer {KEY}"})
    for _ in range(3):
        try:
            with urllib.request.urlopen(req, timeout=35) as resp:
                body = json.load(resp)
        except urllib.error.HTTPError as err:
            if err.code == 429:   # too many requests: wait as long as Hardcount says
                time.sleep(int(err.headers.get("Retry-After", "1")))
                continue
            print(f"{customer_id}: HTTP {err.code}, kept the last figure", file=sys.stderr)
            return None
        except (OSError, ValueError) as err:   # no answer, a timeout, or not JSON
            print(f"{customer_id}: {err}, kept the last figure", file=sys.stderr)
            return None
        entry = next((b for b in body["balances"] if b["currency"] == CURRENCY), None)
        return entry["estimated_remaining_cents"] if entry else 0   # no entry: no credit
    return None


def may_serve(customer_id):
    """Your request path reads only the cache, never Hardcount."""
    left = remaining.get(customer_id)
    return left is None or left > 0   # None: never polled yet. Serve or refuse: your choice.


while True:
    for customer_id in CUSTOMERS:
        figure = read_remaining(customer_id)
        if figure is not None:        # store only a read that worked
            remaining[customer_id] = figure
        print(f"{customer_id} remaining={remaining.get(customer_id)} serve={may_serve(customer_id)}", flush=True)
    time.sleep(60)
```

Save it as `poll_credit.py` and run it with the currency and the customer
IDs to poll:

```bash
export HARDCOUNT=http://localhost:8080
export HARDCOUNT_POLL_KEY=hc_secret_test_…   # a secret key for the poller only
python3 poll_credit.py USD hc_cus_…
```

**You see,** once a minute, one line per customer, such as:

```text
hc_cus_… remaining=3000 serve=True
```

In your app, keep `remaining` in a shared cache instead of the script's
dictionary, and call `may_serve()` from the request path.

## When the read fails

The read answers `500` when Hardcount cannot work the figure out. It never
returns part of a figure. A read that takes longer than 30 seconds answers
`504`, and one over the rate limit answers `429`.

- Keep the last figure you stored for that customer. Do not clear it.
- Go on to the next customer. One failed read must not stop the loop.
- Alert yourself. If a customer's pricing is set up wrongly, the read fails
  for that customer every time until the pricing is fixed.

## A top-up shows at once

Hardcount stores nothing for this read. It works the figure out again on every
call. When you grant credit, the next read already includes it, so poll that
customer again straight after a top-up and update your cache.

## What the figure includes

- Every credit grant, adjustment and expiry.
- Every invoice that has already drawn on the credit.
- The metered usage of the current period that no invoice has billed yet,
  priced with the same rules the invoice will use.
- Each subscription counted over its own billing period, when a customer has
  more than one.

**A grant that has passed its expiry date is not counted**, even before the
expiry run writes the expiry to the ledger. That run waits while a close or
a period invoice that ends before the expiry can still draw on the grant.
Until that close or invoice is settled, `balance_cents` here is lower than
the balance `GET /v1/credits/balance/{customer_id}` returns.

## What the figure leaves out

The figure is an estimate. It can read higher than what the customer really
has left, because these are not subtracted:

- **Tax.** The invoice adds tax; the estimate does not. Credit pays the
  tax too, so with 10% tax, $30.00 of credit pays for about $27.27 of
  usage.
- **A base fee billed at the end of the period.** It appears only when the
  period closes and the invoice is issued.
- **Unpaid invoices.** An invoice that is issued but not yet settled can
  still draw on the credit.
- **Usage sent after a period has ended and before Hardcount has closed it.**
  Hardcount closes a period on the first billing run after
  `INVOICE_GRACE_PERIOD` has passed, so this gap lasts 1 to 2 hours by
  default ([late usage](concepts/usage-events.md#late-usage)).
- **Credit that expires before the period ends.** It counts as spendable
  until its expiry date. If the period's invoice is issued after that date,
  the invoice cannot draw on it.

It can read lower than the truth in one case:

- **A period has ended and its invoice is not built yet.** This lasts
  through `INVOICE_GRACE_PERIOD`, or longer while a failed close waits for
  its next attempt. The estimate subtracts that period's usage but no
  longer counts credit that expired after the period ended. That period's
  invoice still draws on that credit
  ([why](adr/071-credit-expiry-retires-the-block.md)).

Usage sent for a customer who has no active subscription is recorded but not
priced, so it does not lower the figure.

If these gaps matter for your product, stop serving a little above zero
rather than at zero.

<details>
<summary><strong>Edge cases</strong></summary>

Three cases are handled so that nothing is counted twice or wrongly:

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

</details>

## Do not use the credit webhooks for this

The `credit.balance_*` webhooks follow the settled balance, which moves when
an invoice, grant, adjustment or expiry is written, not when usage arrives.
So they can fire weeks after the credit ran out. Use them for top-up
reminders, and the live read to decide whether to serve
([why](webhooks.md#the-credit-events-follow-the-settled-balance)).

## The same figure on the public cost page

The public cost dashboard, `GET /v1/public/cost-dashboard/{token}`, returns
the same three numbers for each currency in its `credits` array. Your
customer sees the figure your backend gates on.

**Next:** how credit is granted, used and expired: [Credits](concepts/credits.md).
Remind customers to top up with the [credit webhooks](webhooks.md#credit-datacredit_balance).
